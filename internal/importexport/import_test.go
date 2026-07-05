package importexport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaildirFolderName(t *testing.T) {
	root := "/mail/user"
	cases := []struct {
		dir  string
		want string
	}{
		{"/mail/user", "INBOX"},
		{"/mail/user/.Sent", "Sent"},
		{"/mail/user/.Sent Items", "Sent"},
		{"/mail/user/.Drafts", "Drafts"},
		{"/mail/user/.Trash", "Trash"},
		{"/mail/user/.spam", "Junk"},
		{"/mail/user/.Work.Projects", "Work/Projects"},
		{"/mail/user/.INBOX.Receipts", "Receipts"},
		{"/mail/user/Work/Projects", "Work/Projects"},
	}
	for _, tc := range cases {
		if got := maildirFolderName(root, tc.dir); got != tc.want {
			t.Errorf("maildirFolderName(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

func TestMaildirFlags(t *testing.T) {
	cases := []struct {
		name string
		want importFlags
	}{
		{"1234567890.M1P2.host", importFlags{}},
		{"1234567890.M1P2.host:2,", importFlags{}},
		{"1234567890.M1P2.host:2,S", importFlags{read: true}},
		{"1234567890.M1P2.host:2,FS", importFlags{read: true, flagged: true}},
		{"1234567890.M1P2.host:2,RST", importFlags{read: true, deleted: true}},
		{"1234567890.M1P2.host:2,DF", importFlags{flagged: true}},
	}
	for _, tc := range cases {
		if got := maildirFlags(tc.name); got != tc.want {
			t.Errorf("maildirFlags(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestDestinationFolderName(t *testing.T) {
	cases := []struct {
		remote string
		delim  string
		want   string
	}{
		{"INBOX", ".", "INBOX"},
		{"inbox", "/", "INBOX"},
		{"INBOX.Sent", ".", "Sent"},
		{"INBOX.Work.Projects", ".", "Work/Projects"},
		{"Sent Messages", "/", "Sent"},
		{"Deleted Items", "/", "Trash"},
		{"Bulk Mail", ".", "Junk"},
		{"Clients/Acme", "/", "Clients/Acme"},
	}
	for _, tc := range cases {
		if got := destinationFolderName(tc.remote, tc.delim); got != tc.want {
			t.Errorf("destinationFolderName(%q, %q) = %q, want %q", tc.remote, tc.delim, got, tc.want)
		}
	}
}

func TestMaildirEntries(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("Subject: x\r\n\r\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cur/100.M1.host:2,S")
	write("new/101.M1.host")
	write("tmp/102.M1.host")
	write(".Sent/cur/103.M1.host:2,S")
	write(".Work.Projects/new/104.M1.host")
	write("dovecot-uidlist")

	entries, err := maildirEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]importFlags{}
	for _, entry := range entries {
		got[entry.folder+"|"+filepath.Base(entry.path)] = entry.flags
	}
	want := map[string]importFlags{
		"INBOX|100.M1.host:2,S":     {read: true},
		"INBOX|101.M1.host":         {},
		"Sent|103.M1.host:2,S":      {read: true},
		"Work/Projects|104.M1.host": {},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries (%v), want %d", len(got), got, len(want))
	}
	for key, flags := range want {
		if actual, ok := got[key]; !ok || actual != flags {
			t.Errorf("entry %q: got %+v ok=%v, want %+v", key, actual, ok, flags)
		}
	}
}
