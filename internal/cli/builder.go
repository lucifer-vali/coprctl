package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/abn/coprctl/internal/cerr"
	ctrruntime "github.com/abn/coprctl/internal/runtime"
)

// Builder produces source RPMs and runs local preflight builds. The concrete
// backend is resolved from the --runtime flag and intent: a container when
// available, otherwise native host tools (spectool + rpmbuild) or mock.
type Builder interface {
	// Name identifies the backend.
	Name() string
	// BuildSRPM produces a source RPM from a spec and returns its path.
	BuildSRPM(ctx context.Context, spec, chroot string, stdout io.Writer) (string, error)
	// Preflight runs the two-stage build (SRPM, then rebuild) for a spec.
	Preflight(ctx context.Context, spec, chroot string, stdout io.Writer) error
}

// runtimeMode is the resolved backend mode.
type runtimeMode string

const (
	modeAuto      runtimeMode = "auto"
	modeContainer runtimeMode = "container"
	modeNative    runtimeMode = "native"
	modeMock      runtimeMode = "mock"
)

// resolveBuilder picks the backend for a command. mode is the --runtime flag
// value (empty means auto). wantSRPM is true when only an SRPM is required (so
// native rpmbuild is a fine fallback); for a full preflight, mock is preferred
// over native when no container is present.
func resolveBuilder(mode, wantSRPM string) (Builder, error) {
	m := normalizeMode(mode)
	switch m {
	case modeContainer:
		return containerBuilder{prefer: ""}, nil
	case modeNative:
		return nativeBuilder{}, nil
	case modeMock:
		if err := (mockBuilder{}).Available(); err != nil {
			return nil, mockError(err)
		}
		return mockBuilder{}, nil
	}
	// auto: container first, then fall back by intent.
	cb := containerBuilder{prefer: ""}
	if err := cb.Available(); err == nil {
		return cb, nil
	}
	if wantSRPM == "srpm" {
		return nativeBuilder{}, nil
	}
	var mb mockBuilder
	if mb.Available() == nil {
		return mb, nil
	}
	return nativeBuilder{}, nil
}

func normalizeMode(mode string) runtimeMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		return modeAuto
	case "container", "podman", "docker":
		return modeContainer
	case "native", "host", "rpmbuild":
		return modeNative
	case "mock":
		return modeMock
	default:
		return modeAuto
	}
}

// detectRuntime resolves the container runtime; overridable in tests.
var detectRuntime = ctrruntime.Detect

// containerBuilder runs the rpmbuilder image via a container runtime.
type containerBuilder struct {
	prefer string
}

func (containerBuilder) Name() string { return "container" }

func (c containerBuilder) Available() error {
	_, err := ctrruntime.Detect(c.prefer)
	return err
}

func (c containerBuilder) run(ctx context.Context, spec, chroot string, env []string, stdout io.Writer) error {
	rt, err := detectRuntime(c.prefer)
	if err != nil {
		return cerr.New("no_runtime", cerr.ExitPrecondition, err.Error())
	}
	m := resolveChrootImage(chroot)
	if m.Match == "none" {
		return cerr.New("no_image", cerr.ExitPrecondition, m.Reason)
	}
	return rt.Run(ctx, ctrruntime.RunSpec{
		Image:   m.Image,
		WorkDir: filepath.Dir(spec),
		Mount:   "/sources",
		Env:     env,
		Args:    []string{"/usr/bin/rpmbuilder"},
		Stdout:  stdout,
	})
}

func (c containerBuilder) BuildSRPM(ctx context.Context, spec, chroot string, stdout io.Writer) (string, error) {
	if err := c.run(ctx, spec, chroot, []string{"SRPM_ONLY=1", "OUTPUT=/sources/.rpmbuild"}, stdout); err != nil {
		return "", err
	}
	return findSRPM(filepath.Dir(spec))
}

func (c containerBuilder) Preflight(ctx context.Context, spec, chroot string, stdout io.Writer) error {
	if err := c.run(ctx, spec, chroot, []string{"SRPM_ONLY=1", "OUTPUT=/sources/.rpmbuild"}, stdout); err != nil {
		return err
	}
	return c.run(ctx, spec, chroot, []string{"FROM_SRPM=1", "SOURCES=/sources/.rpmbuild", "OUTPUT=/sources/.rpmbuild"}, stdout)
}

// nativeBuilder uses spectool + rpmbuild on the host.
type nativeBuilder struct{}

func (nativeBuilder) Name() string { return "native" }

func (nativeBuilder) Available() error {
	for _, bin := range []string{"rpmbuild", "spectool"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found", bin)
		}
	}
	return nil
}

func (b nativeBuilder) BuildSRPM(ctx context.Context, spec, _ string, stdout io.Writer) (string, error) {
	dir := filepath.Dir(spec)
	top := filepath.Join(dir, ".rpmbuild")
	// spectool downloads sources to _sourcedir; point it at the spec dir so
	// rpmbuild reads them from the same place. _topdir keeps build output out
	// of the source tree.
	defs := []string{
		"--define", "_topdir " + top,
		"--define", "_sourcedir " + dir,
	}
	if err := execCmd(ctx, stdout, "spectool", append(defs, "--sourcedir", "--get-files", spec)...); err != nil {
		return "", cerr.New("source_fetch_failed", cerr.ExitBuildFailed, "spectool failed to fetch sources").Wrap(err)
	}
	if err := execCmd(ctx, stdout, "rpmbuild", append(defs, "-bs", spec)...); err != nil {
		return "", cerr.New("srpm_failed", cerr.ExitBuildFailed, "rpmbuild -bs failed").Wrap(err)
	}
	return findSRPM(dir)
}

func (b nativeBuilder) Preflight(ctx context.Context, spec, _ string, stdout io.Writer) error {
	fmt.Fprintf(stdout, "warning: native preflight is not a clean-room buildroot; mock is the higher-fidelity fallback\n")
	dir := filepath.Dir(spec)
	top := filepath.Join(dir, ".rpmbuild")
	defs := []string{
		"--define", "_topdir " + top,
		"--define", "_sourcedir " + dir,
	}
	if err := execCmd(ctx, stdout, "spectool", append(defs, "--sourcedir", "--get-files", spec)...); err != nil {
		return cerr.New("source_fetch_failed", cerr.ExitBuildFailed, "spectool failed to fetch sources").Wrap(err)
	}
	return execCmd(ctx, stdout, "rpmbuild", append(defs, "-ba", spec)...)
}

// mockBuilder uses mock for a clean-room buildroot.
type mockBuilder struct{}

func (mockBuilder) Name() string { return "mock" }

func (mockBuilder) Available() error {
	if _, err := exec.LookPath("mock"); err != nil {
		return fmt.Errorf("mock not found")
	}
	return nil
}

func (b mockBuilder) BuildSRPM(ctx context.Context, spec, chroot string, stdout io.Writer) (string, error) {
	dir := filepath.Dir(spec)
	if err := execCmd(ctx, stdout, "spectool", "--sourcedir", "--get-files", spec); err != nil {
		return "", cerr.New("source_fetch_failed", cerr.ExitBuildFailed, "spectool failed to fetch sources").Wrap(err)
	}
	outDir := filepath.Join(dir, ".rpmbuild")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	// --resultdir captures the SRPM from the clean-root.
	if err := execCmd(ctx, stdout, "mock", "-r", chroot, "--buildsrpm", "--spec", spec,
		"--sources", dir, "--resultdir", outDir); err != nil {
		return "", cerr.New("srpm_failed", cerr.ExitBuildFailed,
			"mock --buildsrpm failed; is mock configured and is the user in the mock group?").Wrap(err)
	}
	return findSRPM(outDir)
}

func (b mockBuilder) Preflight(ctx context.Context, spec, chroot string, stdout io.Writer) error {
	if _, err := b.BuildSRPM(ctx, spec, chroot, stdout); err != nil {
		return err
	}
	dir := filepath.Dir(spec)
	outDir := filepath.Join(dir, ".rpmbuild")
	srpm, err := findSRPM(outDir)
	if err != nil {
		return err
	}
	return execCmd(ctx, stdout, "mock", "-r", chroot, "--rebuild", srpm, "--resultdir", outDir)
}

// execCmd runs an external binary, streaming output to stdout, with ctx
// cancellation.
func execCmd(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdout != nil {
		cmd.Stdout = stdout
		cmd.Stderr = stdout
	}
	return cmd.Run()
}

// mockSetupHint returns instructions when mock is required but unavailable.
func mockSetupHint() string {
	return "mock is required for a clean-room buildroot but is not available. Install it (dnf install mock), add your user to the mock group (usermod -aG mock $USER), and re-login."
}

// mockError wraps a mock availability failure with setup instructions.
func mockError(err error) error {
	return cerr.New("mock_unavailable", cerr.ExitPrecondition, mockSetupHint()).Wrap(err)
}
