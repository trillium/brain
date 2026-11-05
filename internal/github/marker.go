package github

import (
	"regexp"
	"strings"
)

// A pushed issue carries a one-line HTML comment at the end of its body naming
// the brain item it mirrors:
//
//	<!-- brain-sync store=brain id=brain-7k2 -->
//
// The comment is invisible when GitHub renders the issue. It is the second
// half of the link between an item and its issue (the first is the item's
// external_ref): if the stored link is ever lost, a crash between creating
// the issue and saving its URL, a restored backup, a second machine that has
// not synced yet, the marker lets the next run adopt the issue it already
// made instead of creating a duplicate. It also names the owning store, so a
// repository shared between two stores never imports one store's mirrors as
// the other's items.

var markerPattern = regexp.MustCompile(`(?m)[ \t]*<!--\s*brain-sync\s+store=(\S+)\s+id=(\S+)\s*-->[ \t]*\n?`)

// Marker names the brain item a GitHub issue mirrors.
type Marker struct {
	Store string
	ID    string
}

// BuildMarker renders the marker comment for an item.
func BuildMarker(store, id string) string {
	return "<!-- brain-sync store=" + store + " id=" + id + " -->"
}

// ParseMarker finds the marker in an issue body. The last marker wins, so a
// body someone pasted a copy into still names the item that pushed it.
func ParseMarker(body string) (Marker, bool) {
	all := markerPattern.FindAllStringSubmatch(body, -1)
	if len(all) == 0 {
		return Marker{}, false
	}
	last := all[len(all)-1]
	return Marker{Store: last[1], ID: last[2]}, true
}

// StripMarker removes every marker from a body and trims the blank lines the
// marker left behind, returning the text a person wrote.
func StripMarker(body string) string {
	if !strings.Contains(body, "brain-sync") {
		return body
	}
	return strings.TrimRight(markerPattern.ReplaceAllString(body, ""), "\r\n \t")
}

// WithMarker returns body with the item's marker appended on its own line.
// Any marker already in body is replaced, so rewriting a body never stacks
// markers.
func WithMarker(body, store, id string) string {
	text := StripMarker(body)
	marker := BuildMarker(store, id)
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}
