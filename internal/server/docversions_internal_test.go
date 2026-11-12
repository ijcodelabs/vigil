package server

import "testing"

// Every save of a whole document is kept; the text from before the history
// existed becomes version 1; saving the same text again adds nothing; the
// oldest go once there are more than keptVersions.
func TestDocumentVersions(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(settingAlerting, "route: {receiver: old}\n"); err != nil { // saved by an older server
		t.Fatal(err)
	}
	if err := st.SaveDocument(settingAlerting, "route: {receiver: team}\n", originServer, "nora"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDocument(settingAlerting, "route: {receiver: team}\n", originServer, "nora"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDocument(settingAlerting, "route: {receiver: pager}\n", originFiles, "admin-token"); err != nil {
		t.Fatal(err)
	}
	list, err := st.DocumentVersions(settingAlerting)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Version != 3 || list[0].Origin != originFiles || list[0].Author != "admin-token" ||
		list[1].Author != "nora" || list[2].Version != 1 || list[2].Origin != originBefore {
		t.Fatalf("versions: %+v", list)
	}
	if doc, ok, _ := st.DocumentVersion(settingAlerting, 1); !ok || doc != "route: {receiver: old}\n" {
		t.Errorf("version 1: %q %v", doc, ok)
	}
	if cur, _ := st.Setting(settingAlerting, ""); cur != "route: {receiver: pager}\n" {
		t.Errorf("current: %q", cur)
	}
	if _, ok, _ := st.DocumentVersion(settingAlerting, 9); ok {
		t.Error("a version that does not exist was found")
	}
	if l, _ := st.DocumentVersions(settingRules); len(l) != 0 {
		t.Errorf("rule file history mixed in: %+v", l)
	}

	// A first save on a new server keeps no "before" version.
	if err := st.SaveDocument(settingRules, "groups: []\n", originServer, "nora"); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.DocumentVersions(settingRules); len(l) != 1 || l[0].Version != 1 || l[0].Origin != originServer {
		t.Errorf("rules: %+v", l)
	}

	for i := range keptVersions + 5 {
		if err := st.SaveDocument(settingRules, "groups: [] # "+string(rune('a'+i%26))+string(rune('a'+i/26))+"\n", originServer, "nora"); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := st.DocumentVersions(settingRules)
	if len(l) != keptVersions || l[0].Version != keptVersions+6 {
		t.Errorf("after many saves: %d kept, newest v%d", len(l), l[0].Version)
	}
}
