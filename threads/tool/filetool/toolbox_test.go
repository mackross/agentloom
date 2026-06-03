package filetool

import (
	"reflect"
	"testing"

	"github.com/mackross/agentloom/threads/tool"
)

func TestAddToolsComposesCatalogInStableOrder(t *testing.T) {
	cat := tool.NewCatalog()
	if got := AddTools(cat, ToolboxConfig{}); got != cat {
		t.Fatalf("AddTools returned a different catalog")
	}
	snap := cat.Snapshot()
	names := make([]string, 0, len(snap.Offered))
	for _, spec := range snap.Offered {
		names = append(names, spec.Name)
	}
	if want := []string{"read", "write", "apply_patch"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("unexpected tool order: %#v", names)
	}
}

func TestNewCatalogComposesFileTools(t *testing.T) {
	cat := NewCatalog(ToolboxConfig{})
	if cat == nil {
		t.Fatal("NewCatalog returned nil")
	}
	if got := len(cat.Snapshot().Offered); got != 3 {
		t.Fatalf("expected three tools, got %d", got)
	}
}
