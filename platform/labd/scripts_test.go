package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pipeline's shell scripts must agree with labd byte for byte.

func TestPreviewNameMatchesShell(t *testing.T) {
	inputs := []string{"dark-mode", "Dark Mode!", "feature/Login--Page", "--idea--", "Über-Idee", "ÉCOLE",
		"\u212a-thing", "Main2", "main2", "x" + strings.Repeat("y", 40), "x" + strings.Repeat("y", 29), "abcdefghijklmnopqrstuvwxyz012-456",
		"release/2026.10", "release-2026-10", "main", "Main", "prod", "---", "a", "a_b c.d", "UPPER_case-123", "ÜÄÖ", "tab\there",
		"a--b", "-a", "a-", "fire-goal", "dark-mode\n", "dark-mode "}
	for _, in := range inputs {
		goName, goErr := previewName(in)
		out, shErr := exec.Command("bash", "../scripts/preview-name.sh", in).Output()
		shName := strings.TrimSpace(string(out))
		if (goErr == nil) != (shErr == nil) || goName != shName {
			t.Errorf("%q: go %q (%v), shell %q (%v)", in, goName, goErr, shName, shErr)
		}
	}
}

func TestAudienceMatchesShell(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body")
	os.WriteFile(body, []byte("bundle bytes\x00\xff"), 0o644)
	cases := []struct {
		method, uri, body, want string
	}{
		{"POST", "/api/deploy?app=hello&env=prod&commit=abc1234", body, apiAudience("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", sha256Hex([]byte("bundle bytes\x00\xff")))},
		{"GET", "/api/status", "", apiAudience("GET", "/api/status", sha256Hex(nil))},
		{"DELETE", "/api/instances?app=hello&env=dark-mode", "", apiAudience("DELETE", "/api/instances?app=hello&env=dark-mode", sha256Hex(nil))},
		{"POST", "/api/platform", body, platformAudience(sha256Hex([]byte("bundle bytes\x00\xff")))},
	}
	for _, c := range cases {
		args := []string{"../scripts/lab-api.sh", c.method, c.uri}
		if c.body != "" {
			args = append(args, c.body)
		}
		cmd := exec.Command("bash", args...)
		cmd.Env = append(os.Environ(), "LAB_API_PRINT_AUD=1")
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != c.want {
			t.Errorf("%s %s: shell %q (%v), want %q", c.method, c.uri, out, err, c.want)
		}
	}
}

// changes.sh: on main, labd and each app are compared with what the server
// runs, so a change whose run was cancelled or failed still ships (security
// review M3); test harness changes are flagged like test changes (L8).
func TestChangesComparesWithWhatRuns(t *testing.T) {
	for _, tool := range []string{"git", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, content string) {
		p := filepath.Join(repo, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o755)
	}
	script, _ := os.ReadFile("../scripts/changes.sh")
	write("platform/scripts/changes.sh", string(script))
	write("apps/hello/app.json", "{}")
	write("apps/other/app.json", "{}")
	write("platform/labd/main.go", "package main")
	write("go.mod", "module lab")
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "one")
	deployed := git("rev-parse", "HEAD")
	write("apps/hello/main.go", "package main // changed")
	git("add", "-A")
	git("commit", "-qm", "hello changed, but that run was cancelled")
	missed := git("rev-parse", "HEAD")
	write("platform/labd/main.go", "package main // changed")
	git("commit", "-qam", "labd changed")
	write("e2e/playwright.config.ts", "// weaker")
	git("add", "-A")
	git("commit", "-qm", "harness changed")

	run := func(before, running string) map[string]any {
		t.Helper()
		args := []string{filepath.Join(repo, "platform/scripts/changes.sh"), before, "main", "false"}
		if running != "" {
			f := filepath.Join(t.TempDir(), "running.json")
			os.WriteFile(f, []byte(running), 0o644)
			args = append(args, f)
		}
		out, err := exec.Command("bash", args...).Output()
		if err != nil {
			t.Fatalf("changes.sh: %v", err)
		}
		var js map[string]any
		if err := json.Unmarshal(out, &js); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return js
	}
	apps := func(js map[string]any) string {
		var names []string
		for _, a := range js["apps"].([]any) {
			names = append(names, a.(string))
		}
		return strings.Join(names, ",")
	}
	// Against the previous push only, the missed hello change is lost...
	prev := git("rev-parse", "HEAD~1")
	js := run(prev, "")
	if apps(js) != "hello,other" || js["platform"] != false { // e2e/ counts for every app
		t.Fatalf("previous-push base: %v", js)
	}
	// Only the harness changed: no app's own folder did, so no app gets the
	// browser matrix, except hello as the canary (the owner, 2026-11-22).
	if own := js["own"].([]any); len(own) != 0 || js["canary"] != "hello" {
		t.Fatalf("harness-only push: own %v, canary %v", own, js["canary"])
	}
	// ...against what runs, hello and labd ship; other (unchanged since its
	// deploy) only because the harness changed for every app.
	js = run(prev, `{"labd": "`+deployed[:7]+`", "apps": {"hello": "`+deployed+`", "other": "`+missed+`"}}`)
	if apps(js) != "hello,other" || js["platform"] != true {
		t.Fatalf("running base: %v", js)
	}
	if tc := js["tests_changed"].([]any); len(tc) != 1 || tc[0] != "the test harness" {
		t.Fatalf("harness change not flagged: %v", js)
	}
	// An app the server doesn't run yet, or a commit it doesn't know, ships.
	js = run(prev, `{"labd": "dev", "apps": {}}`)
	if apps(js) != "hello,other" || js["platform"] != true {
		t.Fatalf("unknown base: %v", js)
	}
	// Up to date: nothing to do but the harness change.
	head := git("rev-parse", "HEAD")
	write("README.md", "docs")
	git("add", "-A")
	git("commit", "-qm", "docs")
	js = run(head, `{"labd": "`+head+`", "apps": {"hello": "`+head+`", "other": "`+head+`"}}`)
	if apps(js) != "" || js["platform"] != false || len(js["tests_changed"].([]any)) != 0 {
		t.Fatalf("up to date: %v", js)
	}
	// A rolled-back app is held: production runs an older commit, but only a
	// change to the app ships it again.
	js = run(head, `{"labd": "`+head+`", "apps": {"hello": "`+deployed+`", "other": "`+head+`"}, "held": ["hello"]}`)
	if apps(js) != "" {
		t.Fatalf("held app shipped again: %v", js)
	}
	write("apps/hello/main.go", "package main // fixed")
	git("commit", "-qam", "hello fixed")
	js = run(git("rev-parse", "HEAD~1"), `{"labd": "`+head+`", "apps": {"hello": "`+deployed+`", "other": "`+head+`"}, "held": ["hello"]}`)
	if apps(js) != "hello" {
		t.Fatalf("held app not shipped after its own change: %v", js)
	}
	// What production runs never takes away what this push changed (an
	// answer from the server can't make main skip a change).
	js = run(git("rev-parse", "HEAD~1"), `{"labd": "`+git("rev-parse", "HEAD")+`", "apps": {"hello": "`+git("rev-parse", "HEAD")+`", "other": "`+head+`"}}`)
	if apps(js) != "hello" {
		t.Fatalf("this push's change dropped: %v", js)
	}

	// On a branch, every push builds and tests everything the branch changes
	// against main: a push that itself changes nothing (merging main in, a
	// docs fix) must not make the verdict skip the apps an earlier push
	// changed, whose run may have been cancelled by this one.
	mainHead := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", mainHead)
	git("checkout", "-qb", "idea")
	write("apps/other/main.go", "package main // the idea")
	git("add", "-A")
	git("commit", "-qm", "other changed on the branch")
	first := git("rev-parse", "HEAD")
	write("README.md", "docs again")
	git("commit", "-qam", "docs only")
	runBranch := func(before string) map[string]any {
		t.Helper()
		out, err := exec.Command("bash", filepath.Join(repo, "platform/scripts/changes.sh"), before, "idea", "false").Output()
		if err != nil {
			t.Fatalf("changes.sh: %v", err)
		}
		var js map[string]any
		if err := json.Unmarshal(out, &js); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return js
	}
	if js := runBranch(first); apps(js) != "other" || js["platform"] != false {
		t.Fatalf("branch push that changed nothing itself: %v", js)
	}
	// Tests-changed flags still describe this push only.
	write("apps/other/SPEC.md", "# Other")
	git("add", "-A")
	git("commit", "-qm", "spec")
	js = runBranch(git("rev-parse", "HEAD~1"))
	if tc := js["tests_changed"].([]any); len(tc) != 1 || tc[0] != "other" {
		t.Fatalf("spec change on the branch not flagged: %v", js)
	}
	if js = runBranch(git("rev-parse", "HEAD")); len(js["tests_changed"].([]any)) != 0 || apps(js) != "other" {
		t.Fatalf("branch up to date with itself: %v", js)
	}
}

// new-setup-code.sh makes codes labd reads, and prints the SHA-256 labd
// compares with.
func TestNewSetupCodeScript(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		out, err := exec.Command("bash", "../scripts/new-setup-code.sh").Output()
		if err != nil {
			t.Fatal(err)
		}
		var code, sum string
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(line, "code: "); ok {
				code = v
			}
			if v, ok := strings.CutPrefix(line, "sha256: "); ok {
				sum = v
			}
		}
		norm, ok := normalizeSetupCode(code)
		if !ok || len(code) != 19 || sha256Hex([]byte(norm)) != sum || seen[code] {
			t.Fatalf("script made %q / %q", code, sum)
		}
		seen[code] = true
		a := newAuth(t.TempDir())
		if _, err := a.SetSetupCode(sum, false); err != nil || !a.SetupCodeValid(code) {
			t.Fatalf("labd does not take %s: %v", code, err)
		}
	}
}
