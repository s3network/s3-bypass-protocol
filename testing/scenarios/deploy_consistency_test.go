package scenarios

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The installer and the unit file agree on paths, or the service cannot start.
//
// Both bugs here were found by installing on a real VPS, not by reading the
// files: nothing checks that the directory install-server.sh creates is the
// directory the unit asks systemd to make writable. A stray hyphen made
// startup die before ExecStart with "Failed to set up mount namespacing", and
// a comment promising a 0600 config made the file unreadable by the very
// process it was written for. Both looked fine side by side.
//
// So the check is mechanical: every path in the unit must exist among the
// paths the installer creates.
func TestUnitPathsExistInInstaller(t *testing.T) {
	root := repoRoot(t)

	script := readFile(t, root+"/deploy/install-server.sh")
	unit := readFile(t, root+"/deploy/systemd/s3bypass-protocol.service")

	// What the installer creates. Named captures, defaults only — an
	// overridden INSTALL_DIR at install time is the operator's business, and
	// both sides move together only for the defaults.
	created := map[string]string{}
	re := regexp.MustCompile(`(?m)^(INSTALL_DIR|CONFIG_DIR|STATE_DIR)="\$\{\w+:-([^}]+)\}"`)
	for _, m := range re.FindAllStringSubmatch(script, -1) {
		created[m[2]] = m[1]
	}
	if len(created) == 0 {
		t.Fatal("install-server.sh: no INSTALL_DIR/CONFIG_DIR/STATE_DIR defaults found — " +
			"if they moved, fix this test rather than delete it")
	}

	// What the unit references. PrivateTmp and friends are not paths.
	referenced := map[string]string{}
	reUnit := regexp.MustCompile(`(?m)^(WorkingDirectory|ReadOnlyPaths|ReadWritePaths)=(\S+)`)
	for _, m := range reUnit.FindAllStringSubmatch(unit, -1) {
		referenced[m[2]] = m[1]
	}
	if len(referenced) == 0 {
		t.Fatal("unit file: no WorkingDirectory/ReadOnlyPaths/ReadWritePaths found — " +
			"if they moved, fix this test rather than delete it")
	}

	for path, key := range referenced {
		if _, ok := created[path]; !ok {
			t.Errorf("unit references %s=%s, which install-server.sh does not create.\n"+
				"  installer creates: %v\n"+
				"  systemd will fail to sandbox: \"Failed to set up mount namespacing: %s: "+
				"No such file or directory\"",
				key, path, sortedKeys(created), path)
		}
	}
}

// The config must be readable by the user the unit runs as.
//
// A root-owned 0600 config satisfies "keep the credentials private" and still
// makes the service unstartable, because the process is not root. The unit's
// own comment used to promise 0600 while VPS-DEPLOY.ru.md said 0640 root:s3bypass
// all along — two files, one correct answer, and the wrong one shipped.
//
// Only the comment block directly above ExecStartPre is judged: a prose mention
// of 0600 anywhere else (this file's own note, a changelog) is not a promise
// about how the operator must provision the file.
func TestUnitDoesNotPromiseAnUnreadableConfigMode(t *testing.T) {
	root := repoRoot(t)
	unit := readFile(t, root+"/deploy/systemd/s3bypass-protocol.service")

	user := regexp.MustCompile(`(?m)^User=(\S+)`).FindStringSubmatch(unit)
	if user == nil {
		t.Fatal("unit file: no User= directive — cannot judge config readability")
	}
	serviceUser := user[1]
	if serviceUser == "root" || serviceUser == "0" {
		t.Skipf("unit runs as %s, so any config mode is readable", serviceUser)
	}

	comment := commentBlockAbove(unit, "ExecStartPre=")
	if comment == "" {
		t.Fatal("no comment block above ExecStartPre — nothing promises a mode, but " +
			"fix this test if the unit's shape changed")
	}
	// A mention is not a promise. "keep it 0640, NOT 0600" and "0600 would be
	// wrong" both name the bad mode while telling the operator the right one;
	// only a comment that tells them to use 0600 is a defect. So: judge the
	// mode the comment asks for, not the modes it discusses.
	promised := regexp.MustCompile(`(?i)\b(?:use|keep|make|set|owned\s+and)\b[^.\n]{0,20}\b0600\b`)
	if promised.MatchString(comment) {
		t.Errorf("the comment above ExecStartPre tells the operator to use 0600, but the "+
			"service runs as %s and cannot read it: startup dies with \"permission denied\".\n"+
			"  use 0640 root:%s — the mode VPS-DEPLOY.ru.md documents\n"+
			"  comment says: %s",
			serviceUser, serviceUser, strings.TrimSpace(comment))
	}
}

// commentBlockAbove returns the contiguous run of "#" lines ending at the line
// carrying marker, without the marker line itself.
func commentBlockAbove(unit, marker string) string {
	lines := strings.Split(unit, "\n")
	idx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	var block []string
	for i := idx - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			break
		}
		block = append([]string{lines[i]}, block...)
	}
	return strings.Join(block, "\n")
}

func TestInstallerAndUnitAgreeOnStateDirSpelling(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root+"/deploy/install-server.sh")
	unit := readFile(t, root+"/deploy/systemd/s3bypass-protocol.service")

	m := regexp.MustCompile(`(?m)^STATE_DIR="\$\{\w+:-([^}]+)\}"`).FindStringSubmatch(script)
	if m == nil {
		t.Skip("install-server.sh no longer defines STATE_DIR")
	}
	state := m[1]

	// Judge the directives, not the prose: an explanatory comment may name the
	// broken spelling on purpose — that is the point of such a comment.
	declared := map[string]string{}
	reUnit := regexp.MustCompile(`(?m)^(ReadWritePaths|ReadOnlyPaths|WorkingDirectory)=(\S+)`)
	for _, mm := range reUnit.FindAllStringSubmatch(unit, -1) {
		declared[mm[2]] = mm[1]
	}

	rw := declared["/var/lib/s3bypass-protocol"]
	if rw == "" {
		t.Errorf("no unit directive points at STATE_DIR %q; the writable path must be one "+
			"systemd can sandbox, or startup fails with \"Failed to set up mount namespacing\"", state)
	}
	for path, key := range declared {
		if strings.Contains(path, "s3-bypass-protocol") {
			t.Errorf("%s=%s uses the hyphenated spelling, which broke a real install; "+
				"install-server.sh creates %q", key, path, state)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// This test lives in testing/scenarios/, the repo root is two levels up.
	return wd + "/../.."
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
