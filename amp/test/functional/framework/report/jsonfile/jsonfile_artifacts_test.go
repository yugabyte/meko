package jsonfile_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/report/jsonfile"
)

// TestJSONFilePersistsAttachmentBytes: an EventAttachment carrying Content lands
// on disk under <runid>/artifacts/<test>/<name>, and the finished result's JSON
// records the run-relative Path — without mutating the shared (cross-sink)
// Result.
func TestJSONFilePersistsAttachmentBytes(t *testing.T) {
	base := t.TempDir()
	s := jsonfile.New(base)
	if err := s.RunStarted(core.RunInfo{Seed: 9, StartedAt: time.Now()}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}

	content := []byte("E0712 controller crashloop backtrace")
	att := core.Attachment{Name: "controller.log", Kind: "log", Size: int64(len(content))}
	if err := s.ResultEvent(core.ResultEvent{
		Kind: core.EventAttachment, TestName: "functional/amp_features", Attachment: &att, Content: content,
	}); err != nil {
		t.Fatalf("EventAttachment: %v", err)
	}

	res := &core.Result{
		TestName:    "functional/amp_features",
		Suite:       "functional",
		Status:      core.StatusFailed,
		Attachments: []core.Attachment{{Name: "controller.log", Kind: "log", Size: int64(len(content))}},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("EventTestFinished: %v", err)
	}

	wantPath := filepath.Join("artifacts", "functional_amp_features", "controller.log")
	gotBytes, err := os.ReadFile(filepath.Join(s.Dir(), wantPath))
	if err != nil {
		t.Fatalf("read persisted artifact: %v", err)
	}
	if string(gotBytes) != string(content) {
		t.Errorf("persisted bytes = %q, want %q", gotBytes, content)
	}

	raw, err := os.ReadFile(filepath.Join(s.Dir(), "functional_amp_features.json"))
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var doc struct {
		Attachments []struct {
			Name string
			Path string
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(doc.Attachments) != 1 || doc.Attachments[0].Path != "artifacts/functional_amp_features/controller.log" {
		t.Errorf("recorded attachment Path = %+v, want artifacts/functional_amp_features/controller.log", doc.Attachments)
	}
	if res.Attachments[0].Path != "" {
		t.Errorf("shared Result was mutated (Path=%q); sinks must not race on it", res.Attachments[0].Path)
	}
}

// TestJSONFileMetadataOnlyAttachWritesNoBytes: a metadata-only attach (nil
// Content) persists no file and records no Path — behaving exactly as before.
func TestJSONFileMetadataOnlyAttachWritesNoBytes(t *testing.T) {
	base := t.TempDir()
	s := jsonfile.New(base)
	if err := s.RunStarted(core.RunInfo{Seed: 3, StartedAt: time.Now()}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}

	att := core.Attachment{Name: "big.bin", Kind: "artifact", Size: 4096}
	if err := s.ResultEvent(core.ResultEvent{
		Kind: core.EventAttachment, TestName: "suite/meta", Attachment: &att,
	}); err != nil {
		t.Fatalf("EventAttachment: %v", err)
	}
	res := &core.Result{
		TestName:    "suite/meta",
		Status:      core.StatusPassed,
		Attachments: []core.Attachment{{Name: "big.bin", Kind: "artifact", Size: 4096}},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("EventTestFinished: %v", err)
	}

	if _, err := os.Stat(filepath.Join(s.Dir(), "artifacts")); !os.IsNotExist(err) {
		t.Errorf("artifacts dir should not exist for a metadata-only attach (err=%v)", err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "suite_meta.json"))
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var doc struct {
		Attachments []struct{ Path string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(doc.Attachments) != 1 || doc.Attachments[0].Path != "" {
		t.Errorf("metadata-only attachment Path = %+v, want empty", doc.Attachments)
	}
}
