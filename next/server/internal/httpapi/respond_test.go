package httpapi

import "testing"

// PageBounds is Pagination's clamp: page 1 to 1,000,000, size 1 to 200 with
// 20 for none, so a paged query never gets a negative LIMIT or OFFSET.
func TestPageBounds(t *testing.T) {
	for _, c := range []struct{ page, size, wantPage, wantSize int }{
		{1, 20, 1, 20},
		{3, 50, 3, 50},
		{0, 0, 1, 20},
		{-4, -1, 1, 20},
		{2_000_000, 500, 1_000_000, 200},
	} {
		if p, s := PageBounds(c.page, c.size); p != c.wantPage || s != c.wantSize {
			t.Errorf("PageBounds(%d, %d) = %d, %d; want %d, %d", c.page, c.size, p, s, c.wantPage, c.wantSize)
		}
	}
}
