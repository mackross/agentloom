package filetool_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/threads/tool/filetool"
)

func ExampleAddRead() {
	dir, err := os.MkdirTemp("", "filetool-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		panic(err)
	}

	cat := tool.NewCatalog()
	filetool.AddRead(cat, filetool.ReadConfig{CWD: dir})

	dispatch, err := cat.Dispatch(context.Background(), nil, tool.Call{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"notes.txt"}`,
	})
	if err != nil {
		panic(err)
	}
	result := dispatch.Items[0].(threads.ToolCallResult)
	fmt.Print(result.Output)

	// Output:
	// hello
}

func ExampleNewCatalog() {
	cat := filetool.NewCatalog(filetool.ToolboxConfig{})
	for _, spec := range cat.Snapshot().Offered {
		fmt.Println(spec.Name)
	}

	// Output:
	// read
	// write
	// apply_patch
}
