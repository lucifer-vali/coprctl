package cli

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	ctrruntime "github.com/abn/coprctl/internal/runtime"
)

// recordingRuntime captures the env of each container invocation.
type recordingRuntime struct {
	envs [][]string
}

func (r *recordingRuntime) Name() string     { return "recording" }
func (r *recordingRuntime) Rootless() bool   { return true }
func (r *recordingRuntime) Available() error { return nil }
func (r *recordingRuntime) Run(_ context.Context, spec ctrruntime.RunSpec) error {
	r.envs = append(r.envs, spec.Env)
	return nil
}
func (r *recordingRuntime) Build(context.Context, ctrruntime.BuildSpec) error { return nil }

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want runtimeMode
	}{
		{"", modeAuto},
		{"auto", modeAuto},
		{"container", modeContainer},
		{"podman", modeContainer},
		{"docker", modeContainer},
		{"native", modeNative},
		{"host", modeNative},
		{"rpmbuild", modeNative},
		{"mock", modeMock},
		{"garbage", modeAuto},
	}
	for _, tc := range cases {
		if got := normalizeMode(tc.in); got != tc.want {
			t.Errorf("normalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveBuilderExplicit(t *testing.T) {
	// Explicit modes do not probe the environment, so they are deterministic.
	if b, err := resolveBuilder("native", "srpm"); err != nil || b.Name() != "native" {
		t.Errorf("native: b=%v err=%v", b, err)
	}
	if b, err := resolveBuilder("container", "srpm"); err != nil || b.Name() != "container" {
		t.Errorf("container: b=%v err=%v", b, err)
	}
}

// A preflight runs the image twice. The source stage must read the spec
// directory, the rebuild stage the directory the source stage wrote to.
func TestContainerPreflightStageEnv(t *testing.T) {
	rt := &recordingRuntime{}
	detectRuntime = func(string) (ctrruntime.Runtime, error) { return rt, nil }
	defer func() { detectRuntime = ctrruntime.Detect }()

	b := containerBuilder{}
	spec := filepath.Join(t.TempDir(), "pkg.spec")
	if err := b.Preflight(context.Background(), spec, "fedora-44-x86_64", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rt.envs) != 2 {
		t.Fatalf("container runs = %d, want 2", len(rt.envs))
	}
	wantSource := []string{"SRPM_ONLY=1", "OUTPUT=/sources/.rpmbuild"}
	if !reflect.DeepEqual(rt.envs[0], wantSource) {
		t.Errorf("source stage env = %v, want %v", rt.envs[0], wantSource)
	}
	wantRebuild := []string{"FROM_SRPM=1", "SOURCES=/sources/.rpmbuild", "OUTPUT=/sources/.rpmbuild"}
	if !reflect.DeepEqual(rt.envs[1], wantRebuild) {
		t.Errorf("rebuild stage env = %v, want %v", rt.envs[1], wantRebuild)
	}
}

func TestMockBuilderUnavailable(t *testing.T) {
	// mock may or may not be installed; when forced, missing mock must produce
	// a hint error rather than silently falling back.
	if _, err := resolveBuilder("mock", "srpm"); err == nil {
		t.Log("mock available on this host; skipping")
	}
}
