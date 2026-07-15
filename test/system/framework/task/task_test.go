package task_test

import (
	"context"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/task"
)

func TestRegistryAddGetList(t *testing.T) {
	r := task.NewRegistry()
	if err := r.Add(task.Task{Name: "b", Doc: "second"}); err != nil {
		t.Fatalf("Add b: %v", err)
	}
	if err := r.Add(task.Task{Name: "a", Doc: "first"}); err != nil {
		t.Fatalf("Add a: %v", err)
	}

	got, ok := r.Get("a")
	if !ok || got.Doc != "first" {
		t.Errorf("Get a = %+v, %v", got, ok)
	}
	_, ok = r.Get("missing")
	if ok {
		t.Error("Get missing should report false")
	}

	list := r.List()
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Errorf("List not sorted: %+v", list)
	}
}

func TestRegistryRejectsEmptyNameAndDuplicate(t *testing.T) {
	r := task.NewRegistry()
	if err := r.Add(task.Task{Name: ""}); err == nil {
		t.Error("expected an error for an empty name")
	}
	if err := r.Add(task.Task{Name: "dup"}); err != nil {
		t.Fatalf("Add dup: %v", err)
	}
	if err := r.Add(task.Task{Name: "dup"}); err == nil {
		t.Error("expected an error for a duplicate name")
	}
}

func TestTaskRunReceivesDeps(t *testing.T) {
	r := task.NewRegistry()
	var gotLog string
	tk := task.Task{
		Name: "echo",
		Run: func(ctx context.Context, deps task.Deps) error {
			deps.Logf("hello %s", "world")
			return nil
		},
	}
	if err := r.Add(tk); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Get("echo")
	if !ok {
		t.Fatal("echo not registered")
	}
	if err := got.Run(context.Background(), task.Deps{Logf: func(f string, a ...any) { gotLog = f }}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotLog != "hello %s" {
		t.Errorf("Logf format not received: %q", gotLog)
	}
}
