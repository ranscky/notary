package mem0

import (
	"context"
	"errors"
	"fmt"
)

// maxEnumerationPageSize is the largest page_size Mem0 documents for
// POST /v3/memories/. Enumeration requests it explicitly on every page so the
// walk never depends on the platform's default page size, which is not a
// documented guarantee.
const maxEnumerationPageSize = 200

// maxEnumerationPages bounds how many pages GetAllComplete will follow before
// giving up. A server that always reports another page must not be able to
// drive the client forever; hitting the cap is treated exactly like any other
// incomplete walk and surfaces ErrEnumerationIncomplete. The bound is far above
// any legitimate scope (200 * 1000 = 200,000 memories) but finite.
const maxEnumerationPages = 1000

// ErrEnumerationIncomplete reports that an enumeration could not be proven
// exhaustive: the pages Mem0 served did not add up to the total count it
// reported, or the page cap was reached first. Detecting this is the whole
// point of the type: a short read must never be mistaken for a smaller
// complete scope, because a fabricated absence claim is written permanently
// into a signed ledger.
var ErrEnumerationIncomplete = errors.New("mem0: enumeration incomplete")

// enumeration is the verified payload behind a CompleteEnumeration. It is
// reachable only through a non-nil pointer, which is what makes the zero
// CompleteEnumeration recognisable as invalid.
type enumeration struct {
	items []Memory
	count int
}

// CompleteEnumeration is a listing of one scope whose completeness has been
// verified against the total Mem0 reported: len(items) == count was checked
// before the value was constructed. The zero value is invalid — only
// GetAllComplete can construct a usable one — so a consumer cannot fabricate
// an exhaustive-looking empty scope by writing mem0.CompleteEnumeration{}.
// Every accessor on an invalid value returns a zero value rather than
// panicking.
type CompleteEnumeration struct {
	e *enumeration
}

// Valid reports whether c was constructed by GetAllComplete and proven
// complete. The zero value is invalid.
func (c CompleteEnumeration) Valid() bool { return c.e != nil }

// Items returns a copy of the memories in the enumeration — nil when !Valid.
// A copy is returned so that mutating the result cannot alter the underlying
// evidence or change the enumeration's length after its completeness was
// verified.
func (c CompleteEnumeration) Items() []Memory {
	if c.e == nil {
		return nil
	}
	items := make([]Memory, len(c.e.items))
	copy(items, c.e.items)
	return items
}

// Len returns the number of memories collected. It is zero when !Valid; a
// valid enumeration always has Len() == Count().
func (c CompleteEnumeration) Len() int {
	if c.e == nil {
		return 0
	}
	return len(c.e.items)
}

// Count returns the total number of memories Mem0 reported for the filters.
// It is zero when !Valid.
func (c CompleteEnumeration) Count() int {
	if c.e == nil {
		return 0
	}
	return c.e.count
}

// GetAllComplete walks every page of POST /v3/memories/ for the scope in
// req.Filters and returns the memories only once it has proven the listing
// exhaustive: the number of memories collected must equal the count Mem0
// reported as the total for the filter. On any mismatch, on the page cap, or
// on a transport/decode error, it returns a wrapped error (ErrEnumerationIncomplete
// for the completeness failures) together with an invalid CompleteEnumeration,
// never a short list.
//
// Each page requests the documented maximum page_size of 200, and req.Page and
// req.PageSize are ignored: enumeration drives its own pagination so its
// completeness check is the only thing that decides the result.
func (c *Client) GetAllComplete(ctx context.Context, req GetAllRequest) (CompleteEnumeration, error) {
	var items []Memory
	var reported int

	for page := 1; ; page++ {
		if page > maxEnumerationPages {
			return CompleteEnumeration{}, fmt.Errorf(
				"mem0: enumeration exceeded %d pages: %w",
				maxEnumerationPages, ErrEnumerationIncomplete)
		}

		pageReq := req
		pageReq.Page = page
		pageReq.PageSize = maxEnumerationPageSize

		resp, err := c.GetAll(ctx, pageReq)
		if err != nil {
			return CompleteEnumeration{}, err
		}

		// count is the total matching the filters, identical on every page, so
		// the last page read is authoritative for the completeness check.
		reported = resp.Count
		items = append(items, resp.Results...)

		if resp.Next == nil {
			break
		}
	}

	if len(items) != reported {
		return CompleteEnumeration{}, fmt.Errorf(
			"mem0: enumeration collected %d of %d reported memories: %w",
			len(items), reported, ErrEnumerationIncomplete)
	}

	return CompleteEnumeration{e: &enumeration{items: items, count: reported}}, nil
}
