package importexport

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMaildirPath(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "user", "Maildir")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		root      string
		requested string
		want      string
		wantErr   error
	}{
		{name: "relative inside root", root: root, requested: "user/Maildir", want: inside},
		{name: "absolute inside root", root: root, requested: inside, want: inside},
		{name: "root itself", root: root, requested: ".", want: root},
		{name: "dot dot escape", root: root, requested: "../" + filepath.Base(outside), wantErr: ErrMaildirPathOutsideRoot},
		{name: "nested dot dot escape", root: root, requested: "user/../../" + filepath.Base(outside), wantErr: ErrMaildirPathOutsideRoot},
		{name: "absolute outside root", root: root, requested: outside, wantErr: ErrMaildirPathOutsideRoot},
		{name: "sibling prefix", root: root, requested: root + "-other", wantErr: ErrMaildirPathOutsideRoot},
		{name: "symlink escape", root: root, requested: "escape", wantErr: ErrMaildirPathOutsideRoot},
		{name: "disabled", root: "", requested: inside, wantErr: ErrMaildirImportDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMaildirPath(tc.root, tc.requested)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, _ := filepath.EvalSymlinks(tc.want)
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestResolveMaildirPathMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveMaildirPath(root, "missing"); err == nil {
		t.Fatal("expected error for missing path")
	}
}
