package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func zoteroProfileFixture(t *testing.T, root, name, directory, base string) string {
	t.Helper()
	profile := filepath.Join(root, name)
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	dataDir, _ := json.Marshal(directory)
	linkedBase, _ := json.Marshal(base)
	prefs := "user_pref(\"extensions.zotero.useDataDir\", true);\n" +
		"user_pref(\"extensions.zotero.dataDir\", " + string(dataDir) + ");\n" +
		"user_pref(\"extensions.zotero.baseAttachmentPath\", " + string(linkedBase) + ");\n"
	if err := os.WriteFile(filepath.Join(profile, "prefs.js"), []byte(prefs), 0600); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestZoteroPathPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.js")
	text := `// Other preferences and JavaScript are ignored, never evaluated.
user_pref("unrelated", "ignored");
user_pref("extensions.zotero.useDataDir", true);
user_pref("extensions.zotero.dataDir", "C:\\Zotero");
user_pref("extensions.zotero.baseAttachmentPath", "/old");
  user_pref( "extensions.zotero.baseAttachmentPath" , "/论文/\"quoted\"/\u4e2d文" );
run_code();
`
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	prefs, err := readZoteroPathPreferences(path)
	if err != nil || !prefs.useDataDir || prefs.dataDir != `C:\Zotero` || prefs.linkedBase != "/论文/\"quoted\"/中文" {
		t.Fatalf("preferences: %+v, %v", prefs, err)
	}
	for _, invalid := range []string{
		`user_pref("extensions.zotero.baseAttachmentPath", getPath());`,
		`user_pref("extensions.zotero.useDataDir", "true");`,
		`user_pref("extensions.zotero.baseAttachmentPath", "/injected"); run_code();`,
	} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readZoteroPathPreferences(path); err == nil {
			t.Fatalf("accepted invalid preference: %s", invalid)
		}
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (4<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readZoteroPathPreferences(path); err == nil {
		t.Fatal("accepted oversized preferences")
	}
}

func TestZoteroProfileDiscovery(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ini := "[Profile0]\nIsRelative=1\nPath=Profiles/relative.default\n" +
		"[Profile1]\nPath=" + outside + "\nIsRelative=0\n" +
		"[General]\nPath=Profiles/not-a-profile\n" +
		"[Profile2]\nIsRelative=1\nPath=../escape\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0600); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "Profiles", "relative.default"), outside}
	if got := zoteroProfileDirectories(root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered profiles: %v", got)
	}
	if err := os.Remove(filepath.Join(root, "profiles.ini")); err != nil {
		t.Fatal(err)
	}
	mac := zoteroProfileFixture(t, root, "Profiles/mac.default", t.TempDir(), t.TempDir())
	linux := zoteroProfileFixture(t, root, "linux.default", t.TempDir(), t.TempDir())
	if got := zoteroProfileDirectories(root); !reflect.DeepEqual(got, []string{mac, linux}) {
		t.Fatalf("standard layouts: %v", got)
	}
	for _, tc := range []struct{ platform, appData, want string }{
		{"darwin", "", filepath.Join(root, "Library", "Application Support", "Zotero")},
		{"linux", "", filepath.Join(root, ".zotero", "zotero")},
		{"windows", outside, filepath.Join(outside, "Zotero", "Zotero")},
		{"windows", "", filepath.Join(root, "AppData", "Roaming", "Zotero", "Zotero")},
	} {
		if got := zoteroProfileRoot(tc.platform, root, tc.appData); got != tc.want {
			t.Fatalf("%s profile root: %s", tc.platform, got)
		}
	}
}

func TestZoteroLinkedBaseMatchesLibrary(t *testing.T) {
	root, directory, base := t.TempDir(), t.TempDir(), t.TempDir()
	other := zoteroProfileFixture(t, root, "other", t.TempDir(), t.TempDir())
	matching := zoteroProfileFixture(t, root, "matching", directory, base)
	resolvedBase, _ := filepath.EvalSymlinks(base)
	got, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{other, matching})
	if err != nil || got != resolvedBase {
		t.Fatalf("matching library: %s, %v", got, err)
	}
	// A copied database must not borrow the original library's configuration.
	if _, err := zoteroLinkedBaseFromProfiles(t.TempDir(), directory, []string{matching}); err == nil {
		t.Fatal("unrelated library accepted")
	}
	alias := filepath.Join(root, "library-alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if got, err := zoteroLinkedBaseFromProfiles(alias, t.TempDir(), []string{matching}); err != nil || got != resolvedBase {
		t.Fatalf("symlinked library: %s, %v", got, err)
	}
	duplicate := zoteroProfileFixture(t, root, "duplicate", directory, base)
	if got, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{matching, duplicate}); err != nil || got != resolvedBase {
		t.Fatalf("equivalent profiles: %s, %v", got, err)
	}
	conflicting := zoteroProfileFixture(t, root, "conflicting", directory, t.TempDir())
	if _, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{matching, conflicting}); err == nil || !strings.Contains(err.Error(), "多个") {
		t.Fatalf("conflicting profiles: %v", err)
	}
	for _, base := range []string{"", "relative", filepath.Join(root, "missing")} {
		profile := zoteroProfileFixture(t, root, "invalid", directory, base)
		if _, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{profile}); err == nil {
			t.Fatalf("accepted invalid base: %q", base)
		}
	}
	// Without a custom data directory preference, match the standard data directory.
	if err := os.WriteFile(filepath.Join(matching, "prefs.js"), []byte("user_pref(\"extensions.zotero.baseAttachmentPath\", "+string(mustJSON(base))+");\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := zoteroLinkedBaseFromProfiles(directory, directory, []string{matching}); err != nil || got != resolvedBase {
		t.Fatalf("default library: %s, %v", got, err)
	}
}

func TestZoteroAutoLinkedBaseScanAndImport(t *testing.T) {
	directory, db := zoteroFixture(t)
	base, root := t.TempDir(), t.TempDir()
	profile := zoteroProfileFixture(t, root, "Profiles/linked.default", directory, base)
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte("[Profile0]\nIsRelative=1\nPath=Profiles/linked.default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "论文"), 0700); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(base, "论文", "linked.pdf")
	if err := os.WriteFile(pdf, testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	resolvedPDF, err := filepath.EvalSymlinks(pdf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE itemAttachments SET path='attachments:论文/linked.pdf',linkMode=2 WHERE itemID=2;
INSERT INTO items VALUES(7,2,1,'LINKED02','2024');
INSERT INTO itemAttachments VALUES(7,1,'attachments:论文\linked.pdf',2,'application/pdf');`); err != nil {
		t.Fatal(err)
	}
	prefsBefore, _ := os.ReadFile(filepath.Join(profile, "prefs.js"))
	dbBefore, _ := os.ReadFile(filepath.Join(directory, "zotero.sqlite"))
	calls := 0
	resolve := func(selected string) (string, error) {
		calls++
		return zoteroLinkedBaseFromProfiles(selected, t.TempDir(), zoteroProfileDirectories(root))
	}
	scan, err := readZoteroWithBaseResolver(context.Background(), directory, "", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(scan.Entries) != 2 || len(scan.Warnings) != 1 || !strings.Contains(scan.Warnings[0], base) {
		t.Fatalf("auto discovery: calls=%d, scan=%+v", calls, scan)
	}
	for _, entry := range scan.Entries {
		if entry.Issue != "" || entry.path != resolvedPDF {
			t.Fatalf("linked entry: %+v", entry)
		}
	}
	s := testServer(t)
	result, err := s.importZoteroEntry(context.Background(), scan.Entries[0])
	if err != nil || result.Status != "imported" {
		t.Fatalf("import: %+v, %v", result, err)
	}
	prefsAfter, _ := os.ReadFile(filepath.Join(profile, "prefs.js"))
	dbAfter, _ := os.ReadFile(filepath.Join(directory, "zotero.sqlite"))
	if !bytes.Equal(prefsBefore, prefsAfter) || !bytes.Equal(dbBefore, dbAfter) {
		t.Fatal("source configuration or database changed")
	}
	// Manual input takes precedence, even when automatic discovery cannot resolve a base.
	scan, err = readZoteroWithBaseResolver(context.Background(), directory, base, func(string) (string, error) {
		t.Fatal("manual override invoked discovery")
		return "", nil
	})
	if err != nil || scan.Entries[0].Issue != "" || len(scan.Warnings) != 0 {
		t.Fatalf("manual override: %+v, %v", scan, err)
	}
	for _, path := range []string{`attachments:..\escape.pdf`, `attachments:论文\..\..\escape.pdf`} {
		if _, _, err := zoteroAttachmentPath(directory, base, "PDF00001", path, 2); err == nil {
			t.Fatalf("accepted traversal: %s", path)
		}
	}
}

func TestZoteroAutoLinkedBaseFailureKeepsOtherAttachments(t *testing.T) {
	directory, db := zoteroFixture(t)
	if _, err := db.Exec(`INSERT INTO items VALUES(7,2,1,'LINKED02','2024'),(8,2,1,'LINKED03','2024');
INSERT INTO itemAttachments VALUES(7,1,'attachments:missing.pdf',2,'application/pdf'),(8,1,'attachments:also-missing.pdf',2,'application/pdf');`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	scan, err := readZoteroWithBaseResolver(context.Background(), directory, "", func(selected string) (string, error) {
		calls++
		return zoteroLinkedBaseFromProfiles(selected, t.TempDir(), nil)
	})
	if err != nil || calls != 1 || len(scan.Entries) != 3 {
		t.Fatalf("scan: %+v, calls=%d, err=%v", scan, calls, err)
	}
	if scan.Entries[0].Issue != "" || !strings.Contains(scan.Entries[1].Issue, "手动填写") || scan.Entries[1].Issue != scan.Entries[2].Issue {
		t.Fatalf("attachment issues: %+v", scan.Entries)
	}
	// Neither stored PDFs nor absolute linked PDFs need profile discovery.
	absolute := filepath.Join(t.TempDir(), "absolute.pdf")
	if err := os.WriteFile(absolute, testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE itemAttachments SET path=? WHERE itemID IN (7,8)`, absolute); err != nil {
		t.Fatal(err)
	}
	scan, err = readZoteroWithBaseResolver(context.Background(), directory, "", func(string) (string, error) {
		t.Fatal("stored or absolute attachment invoked discovery")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range scan.Entries {
		if entry.Issue != "" {
			t.Fatalf("ordinary attachment: %+v", entry)
		}
	}
}
