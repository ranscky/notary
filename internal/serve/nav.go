package serve

// The console's sections, named once. A page carries one of these as its Current,
// and the nav marks the entry whose Section matches -- so "which part of the
// console am I in" has a single spelling rather than a string literal repeated in
// every handler and every template.
const (
	sectionOverview = "overview"
	sectionRecords  = "records"
	sectionGaps     = "gaps"
	sectionVerify   = "verify"
)

// navEntry is one link in the console's nav bar.
type navEntry struct {
	Href    string
	Label   string
	Section string
}

// navEntries is the nav bar, in the order it renders.
//
// It is a list in one place on purpose: every page shows the same bar, so a page
// added later is one line here rather than an edit to a template that four other
// pages would then have to be re-checked against. Nothing else in the package
// enumerates the console's sections.
var navEntries = []navEntry{
	{Href: "/", Label: "Overview", Section: sectionOverview},
	{Href: "/records", Label: "Records", Section: sectionRecords},
	{Href: "/gaps", Label: "Gaps", Section: sectionGaps},
	{Href: "/verify", Label: "Verify", Section: sectionVerify},
}

// navItem is one rendered link: an entry, plus whether the current page is in its
// section.
type navItem struct {
	Href    string
	Label   string
	Current bool
}

// navFor returns the nav bar with the page's own section marked.
//
// It takes the page's Current key rather than a page name, so the layout needs no
// knowledge of sections: it ranges over the result and renders aria-current, which
// is what makes the marker announce itself to a screen reader instead of being
// conveyed by colour alone.
func navFor(current string) []navItem {
	items := make([]navItem, 0, len(navEntries))
	for _, e := range navEntries {
		items = append(items, navItem{Href: e.Href, Label: e.Label, Current: e.Section == current})
	}
	return items
}
