package rows

import (
	"reflect"
	"testing"

	"gorm.io/gorm"
)

// Change adds the two columns to a copy: the caller's map stays as it was.
func TestChangeLeavesSetAlone(t *testing.T) {
	set := map[string]any{"status": "enabled"}
	got := Change(set)
	if len(set) != 1 || len(got) != 3 || got["status"] != "enabled" {
		t.Fatalf("set %v, change %v", set, got)
	}
	if !reflect.DeepEqual(got["row_version"], gorm.Expr("row_version + 1")) || !reflect.DeepEqual(got["updated_at"], gorm.Expr("now()")) {
		t.Fatalf("change %v", got)
	}
	if got := Change(nil); len(got) != 2 {
		t.Fatalf("Change(nil) = %v", got)
	}
}
