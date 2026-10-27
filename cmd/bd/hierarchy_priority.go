package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/validation"
	"gopkg.in/yaml.v3"
)

// hierarchyReader is the store surface the specificity gate needs.
// storage.DoltStorage and storage.Transaction both satisfy it.
type hierarchyReader interface {
	GetConfig(ctx context.Context, key string) (string, error)
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
}

type hierarchyViolation struct {
	ChildID        string `json:"child_id"`
	ChildPriority  int    `json:"child_priority"`
	ParentID       string `json:"parent_id"`
	ParentPriority int    `json:"parent_priority"`
}

func (v hierarchyViolation) line() string {
	return fmt.Sprintf("child %s priority %d parent %s priority %d",
		v.ChildID, v.ChildPriority, v.ParentID, v.ParentPriority)
}

// hierarchyBeadsDir is the store's .beads directory, which holds config.yaml.
// StoreLocator.CLIDir is the dolt database directory, not that file, so the
// gate reads filepath.Dir(Path()) when config.yaml is there.
func hierarchyBeadsDir(st hierarchyReader) string {
	if st == nil {
		return ""
	}
	candidate := any(st)
	if ds, ok := st.(storage.DoltStorage); ok {
		candidate = storage.UnwrapStore(ds)
	}
	loc, ok := candidate.(storage.StoreLocator)
	if !ok {
		return ""
	}
	path := loc.Path()
	if path == "" {
		return ""
	}
	for _, dir := range []string{filepath.Dir(path), path} {
		if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
			return dir
		}
	}
	return ""
}

// hierarchyPriorityMode reads hierarchy.priority from the same place
// bd config get reads it. The hierarchy. namespace is yaml-only, so a SQL
// GetConfig would never see `bd config set hierarchy.priority specificity`.
//
// The store file is preferred when it actually contains the key, including the
// flat dotted form (`hierarchy.priority: specificity`) that a nested-only walk
// misses. Otherwise the merged viper value is used, which is what `bd config get`
// prints.
func hierarchyPriorityMode(ctx context.Context, st hierarchyReader) (string, error) {
	key := validation.HierarchyPriorityKey
	if config.IsYamlOnlyKey(key) {
		if dir := hierarchyBeadsDir(st); dir != "" {
			if v, ok := readYAMLConfigKey(filepath.Join(dir, "config.yaml"), key); ok {
				return v, nil
			}
		}
		return config.GetYamlConfig(key), nil
	}
	if st == nil {
		return "", fmt.Errorf("read %s: store is not open", key)
	}
	return st.GetConfig(ctx, key)
}

// readYAMLConfigKey reads a dotted key from a config.yaml file. It accepts the
// flat form (`hierarchy.priority: specificity`) and the nested form. ok is
// false when the file or the key is absent.
func readYAMLConfigKey(path, key string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", false
	}
	if raw, ok := root[key]; ok {
		return yamlScalarString(raw)
	}
	var node interface{} = root
	for _, part := range strings.Split(key, ".") {
		m, ok := node.(map[string]interface{})
		if !ok {
			return "", false
		}
		node, ok = m[part]
		if !ok {
			return "", false
		}
	}
	return yamlScalarString(node)
}

func yamlScalarString(v interface{}) (string, bool) {
	switch s := v.(type) {
	case nil:
		return "", false
	case string:
		return s, true
	default:
		return fmt.Sprintf("%v", s), true
	}
}

// rejectHierarchyInversion returns an error when the specificity gate is on
// and childPriority is not more specific than the parent's priority.
// A missing parent is left to the caller's existing not-found path.
// external: refs are not issues in this store and are skipped.
func rejectHierarchyInversion(ctx context.Context, st hierarchyReader, childID string, childPriority int, parentID string) error {
	return rejectHierarchyInversionFrom(ctx, st, st, childID, childPriority, parentID)
}

// rejectHierarchyInversionFrom reads the gate from configSrc and the parent
// issue from issueSrc. Graph apply and bulk dep add pass the open store as
// configSrc (it knows the .beads directory) and the transaction as issueSrc
// (it can see rows created earlier in the same transaction).
func rejectHierarchyInversionFrom(ctx context.Context, configSrc, issueSrc hierarchyReader, childID string, childPriority int, parentID string) error {
	if issueSrc == nil || childID == "" || parentID == "" {
		return nil
	}
	if strings.HasPrefix(childID, "external:") || strings.HasPrefix(parentID, "external:") {
		return nil
	}
	mode, err := hierarchyPriorityMode(ctx, configSrc)
	if err != nil {
		return err
	}
	if !validation.HierarchySpecificityEnabled(mode) {
		return nil
	}
	parent, err := issueSrc.GetIssue(ctx, parentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	if parent == nil {
		return nil
	}
	if validation.ChildMayFollowParent(parent.Priority, childPriority) {
		return nil
	}
	return validation.HierarchySpecificityError(childID, childPriority, parent.ID, parent.Priority)
}

// rejectParentChildEdgeByIDs loads the child's priority, then applies the gate.
// A missing child is left to the caller's existing not-found path.
func rejectParentChildEdgeByIDs(ctx context.Context, st hierarchyReader, childID, parentID string) error {
	return rejectParentChildEdgeByIDsFrom(ctx, st, st, childID, parentID)
}

// rejectParentChildEdgeByIDsFrom is rejectParentChildEdgeByIDs with separate
// config and issue readers. See rejectHierarchyInversionFrom.
func rejectParentChildEdgeByIDsFrom(ctx context.Context, configSrc, issueSrc hierarchyReader, childID, parentID string) error {
	if issueSrc == nil || childID == "" || parentID == "" {
		return nil
	}
	if strings.HasPrefix(childID, "external:") || strings.HasPrefix(parentID, "external:") {
		return nil
	}
	mode, err := hierarchyPriorityMode(ctx, configSrc)
	if err != nil {
		return err
	}
	if !validation.HierarchySpecificityEnabled(mode) {
		return nil
	}
	child, err := issueSrc.GetIssue(ctx, childID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	if child == nil {
		return nil
	}
	return rejectHierarchyInversionFrom(ctx, configSrc, issueSrc, child.ID, child.Priority, parentID)
}

func hierarchyReportPrefixes(ctx context.Context, st storage.DoltStorage) ([]string, error) {
	var prefixes []string
	seen := map[string]struct{}{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, "-")
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		prefixes = append(prefixes, p)
	}

	primary := ""
	if dir := hierarchyBeadsDir(st); dir != "" {
		primary = strings.TrimSpace(config.GetStringFromDir(dir, "issue-prefix"))
	}
	if primary == "" {
		primary = strings.TrimSpace(config.GetYamlConfig("issue-prefix"))
	}
	if primary == "" {
		dbPrefix, err := st.GetConfig(ctx, "issue_prefix")
		if err != nil {
			return nil, fmt.Errorf("read issue_prefix: %w", err)
		}
		primary = dbPrefix
	}
	add(primary)

	allowed, err := st.GetConfig(ctx, "allowed_prefixes")
	if err != nil {
		return nil, fmt.Errorf("read allowed_prefixes: %w", err)
	}
	for _, p := range strings.Split(allowed, ",") {
		add(p)
	}
	return prefixes, nil
}

func idInHierarchyPrefixes(id string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(id, p+"-") || strings.HasPrefix(id, p+".") {
			return true
		}
	}
	return false
}

func hierarchyViolations(ctx context.Context, st storage.DoltStorage) ([]hierarchyViolation, error) {
	prefixes, err := hierarchyReportPrefixes(ctx, st)
	if err != nil {
		return nil, err
	}
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*types.Issue, len(issues))
	for _, iss := range issues {
		if iss == nil || iss.ID == "" {
			continue
		}
		byID[iss.ID] = iss
	}
	records, err := st.GetAllDependencyRecords(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []hierarchyViolation
	for _, deps := range records {
		for _, dep := range deps {
			if dep == nil || dep.Type != types.DepParentChild {
				continue
			}
			if strings.HasPrefix(dep.IssueID, "external:") || strings.HasPrefix(dep.DependsOnID, "external:") {
				continue
			}
			child := byID[dep.IssueID]
			parent := byID[dep.DependsOnID]
			if child == nil || parent == nil {
				continue
			}
			if !idInHierarchyPrefixes(child.ID, prefixes) || !idInHierarchyPrefixes(parent.ID, prefixes) {
				continue
			}
			if validation.ChildMayFollowParent(parent.Priority, child.Priority) {
				continue
			}
			key := child.ID + "\x00" + parent.ID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, hierarchyViolation{
				ChildID:        child.ID,
				ChildPriority:  child.Priority,
				ParentID:       parent.ID,
				ParentPriority: parent.Priority,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ParentID != out[j].ParentID {
			return out[i].ParentID < out[j].ParentID
		}
		return out[i].ChildID < out[j].ChildID
	})
	return out, nil
}

var lintHierarchyCmd = &cobra.Command{
	Use:   "hierarchy",
	Short: "Report parent-child edges that break the specificity ladder",
	Long: `Report parent-child edges that break the specificity ladder.

The check runs only when this store's config.yaml has
hierarchy.priority=specificity. Unset, it prints nothing and exits 0.
Other stores are unchanged.

P0 is the broadest capability. Each child along a parent-child edge uses
a higher priority number, through P4. A parent at 4 may have a child at 4.
Skips are legal. This command lists violating edges and writes nothing.
It exits non-zero when any violation exists.

Set the key with:
  bd config set hierarchy.priority specificity
`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := rootCtx
		mode, err := hierarchyPriorityMode(ctx, store)
		if err != nil {
			return HandleError("%v", err)
		}
		if !validation.HierarchySpecificityEnabled(mode) {
			return nil
		}
		if store == nil {
			return HandleErrorWithHint("database not initialized", diagHint())
		}
		violations, err := hierarchyViolations(ctx, store)
		if err != nil {
			return HandleError("%v", err)
		}
		if len(violations) == 0 {
			return nil
		}
		if jsonOutput {
			if err := outputJSON(violations); err != nil {
				return err
			}
			return SilentExit()
		}
		for _, v := range violations {
			fmt.Println(v.line())
		}
		return SilentExit()
	},
}

func init() {
	lintCmd.AddCommand(lintHierarchyCmd)
}
