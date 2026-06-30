package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
)

// fakeSettings wraps db.FakeStore to satisfy the Settings interface used by resolveModel.
type fakeSettings struct{ store *db.FakeStore }

func (f *fakeSettings) GetSetting(ctx context.Context, key string) (string, error) {
	return f.store.GetSetting(ctx, key)
}

func TestResolveModel_UsesSettingWhenPresent(t *testing.T) {
	store := db.NewFake()
	ctx := context.Background()
	_ = store.SetGlobalRetention(ctx, db.SetGlobalRetentionParams{}) // just to confirm store works

	// Manually set a classify_model setting via the fake.
	fs := &fakeSettings{store: db.NewFake()}

	// Seed a setting via the SetSetting-compatible approach: since FakeStore has no
	// SetSetting, we use the exported method on *db.Store which calls DynamoDB.
	// Instead, build a small in-process Settings that returns a known value.
	fixed := &fixedSettings{key: SettingClassifyModel, val: "us.amazon.nova-lite-v1:0"}
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: fixed}

	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-lite-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-lite-v1:0", got)
	}
	_ = fs
}

func TestResolveModel_FallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	// Settings returns an error (no value set).
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: &fixedSettings{}}
	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-micro-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-micro-v1:0", got)
	}
}

func TestResolveModel_NilSettings(t *testing.T) {
	ctx := context.Background()
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: nil}
	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-micro-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-micro-v1:0", got)
	}
}

func TestResolveModel_ClassifyAndImproveAreIndependent(t *testing.T) {
	ctx := context.Background()
	multi := &multiSettings{vals: map[string]string{
		SettingClassifyModel: "us.amazon.nova-lite-v1:0",
		SettingImproveModel:  "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	}}
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: multi}

	classify := c.resolveModel(ctx, SettingClassifyModel)
	improve := c.resolveModel(ctx, SettingImproveModel)

	if classify != "us.amazon.nova-lite-v1:0" {
		t.Errorf("classify = %q, want nova-lite", classify)
	}
	if improve != "us.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("improve = %q, want claude-haiku", improve)
	}
}

// ---- test helpers ----

type fixedSettings struct {
	key string
	val string
}

func (f *fixedSettings) GetSetting(_ context.Context, key string) (string, error) {
	if key == f.key && f.val != "" {
		return f.val, nil
	}
	return "", errNotFound
}

type multiSettings struct{ vals map[string]string }

func (m *multiSettings) GetSetting(_ context.Context, key string) (string, error) {
	if v, ok := m.vals[key]; ok {
		return v, nil
	}
	return "", errNotFound
}

var errNotFound = errors.New("not found")
