package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDatapackWrites(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	def, err := s.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, gerr := s.DefaultDatapackID(ctx); gerr != nil || got != def {
		t.Fatalf("default %q, %v", got, gerr)
	}

	d, err := s.CreateDatapack(ctx, "project", "about it")
	if err != nil || d.Name != "project" || d.Description != "about it" || d.ID == "" || d.CreatedAt.IsZero() {
		t.Fatalf("create %+v, %v", d, err)
	}
	if _, err = s.CreateDatapack(ctx, "project", ""); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("duplicate name: %v", err)
	}

	name, desc := "renamed", "new words"
	u, err := s.UpdateDatapack(ctx, d.ID, &name, nil)
	if err != nil || u.Name != "renamed" || u.Description != "about it" {
		t.Fatalf("rename %+v, %v", u, err)
	}
	if u, err = s.UpdateDatapack(ctx, d.ID, nil, &desc); err != nil || u.Name != "renamed" || u.Description != desc {
		t.Fatalf("describe %+v, %v", u, err)
	}
	if _, err = s.UpdateDatapack(ctx, d.ID, ptr(DefaultDatapackName), nil); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("rename onto another's name: %v", err)
	}
	if _, err = s.UpdateDatapack(ctx, "nope", &name, nil); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("update a missing datapack: %v", err)
	}
	if got, gerr := s.GetDatapack(ctx, d.ID); gerr != nil || got.Name != "renamed" {
		t.Errorf("get %+v, %v", got, gerr)
	}

	conv := Conversation{ID: "c1", DatapackID: d.ID, AgentID: "a", Title: "t"}
	if _, _, err = s.CreateConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if c, cerr := s.CountDatapack(ctx, d.ID, "u"); cerr != nil || c.Conversations != 1 || c.Memories != 0 {
		t.Errorf("counts %+v, %v", c, cerr)
	}
	if err = s.DeleteDatapack(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetDatapack(ctx, d.ID); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("deleted datapack: %v", err)
	}
	if _, err = s.GetConversation(ctx, d.ID, "c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("its conversation should go with it: %v", err)
	}
	if err = s.DeleteDatapack(ctx, d.ID); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("delete twice: %v", err)
	}

	// The default, once deleted, is gone for good.
	if err = s.DeleteDatapack(ctx, def); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DefaultDatapackID(ctx); err != nil || got != "" {
		t.Errorf("default after deleting it: %q, %v", got, err)
	}
}

// Searches take at most SearchSlots readers, so plain reads never wait behind them.
func TestSearchesLeaveReadersFree(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, SearchSlots)
	for range SearchSlots {
		release, err := s.searchSlot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	// Every slot is taken: a search waits, and gives up when its context ends.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.KeywordSearch(short, "d", "anything", 5, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a search past the slots: %v", err)
	}
	// Plain reads don't wait.
	start := time.Now()
	for range Readers {
		if _, err := s.Datapacks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("reads took %v with every search slot held", d)
	}
	for _, r := range releases {
		r()
	}
	if _, err := s.KeywordSearch(ctx, "d", "anything", 5, nil); err != nil {
		t.Errorf("a search with slots free: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
