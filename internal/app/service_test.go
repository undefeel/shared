package app

import (
	"encoding/xml"
	"strings"
	"testing"
)

type plistData struct {
	XMLName xml.Name `xml:"plist"`
	Dict    dictData `xml:"dict"`
}

type dictData struct {
	Keys    []string   `xml:"key"`
	Strings []string   `xml:"string"`
	Array   *arrayData `xml:"array"`
	Dict    *envDict   `xml:"dict"`
}

type arrayData struct {
	Strings []string `xml:"string"`
}

type envDict struct {
	Key    string `xml:"key"`
	String string `xml:"string"`
}

func TestLaunchdPlist(t *testing.T) {
	exe := "/Applications/Test & Foo <bar>/sharedir"
	home := "/Users/test & user <name>"

	plistStr := launchdPlist(exe, home)

	var parsed plistData
	if err := xml.Unmarshal([]byte(plistStr), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v\ncontent:\n%s", err, plistStr)
	}

	foundExe := false
	if parsed.Dict.Array != nil {
		for _, s := range parsed.Dict.Array.Strings {
			if s == exe {
				foundExe = true
				break
			}
		}
	}
	if !foundExe {
		t.Errorf("expected exe %q in ProgramArguments array, got %v", exe, parsed.Dict.Array)
	}

	if parsed.Dict.Dict == nil || parsed.Dict.Dict.String != home {
		t.Errorf("expected home %q in EnvironmentVariables, got %v", home, parsed.Dict.Dict)
	}
}

func TestSystemdUnit(t *testing.T) {
	exe := "/путь с пробелом/sharedir"
	home := "/home/путь с пробелом"

	unit, err := systemdUnit(exe, home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedExec := `ExecStart="/путь с пробелом/sharedir" run`
	if !strings.Contains(unit, expectedExec) {
		t.Errorf("expected unit to contain %q, got:\n%s", expectedExec, unit)
	}

	expectedEnv := `Environment="SHAREDIR_HOME=/home/путь с пробелом"`
	if !strings.Contains(unit, expectedEnv) {
		t.Errorf("expected unit to contain %q, got:\n%s", expectedEnv, unit)
	}

	// Escaping check
	exeEsc := `/path "with quotes" and \slash and %percent/sharedir`
	homeEsc := `/home "user" %dir`
	unitEsc, err := systemdUnit(exeEsc, homeEsc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedExecEsc := `ExecStart="/path \"with quotes\" and \\slash and %%percent/sharedir" run`
	if !strings.Contains(unitEsc, expectedExecEsc) {
		t.Errorf("expected unit to contain %q, got:\n%s", expectedExecEsc, unitEsc)
	}
	expectedEnvEsc := `Environment="SHAREDIR_HOME=/home \"user\" %%dir"`
	if !strings.Contains(unitEsc, expectedEnvEsc) {
		t.Errorf("expected unit to contain %q, got:\n%s", expectedEnvEsc, unitEsc)
	}

	// Newline test
	for _, badExe := range []string{"/path\nwith/newline", "/path\rwith/return"} {
		if _, err := systemdUnit(badExe, "/home/test"); err == nil {
			t.Errorf("expected error for newline in exe %q, got nil", badExe)
		} else if !strings.Contains(err.Error(), "путь содержит перевод строки") {
			t.Errorf("expected 'путь содержит перевод строки' error, got %v", err)
		}
	}

	for _, badHome := range []string{"/home\ntest", "/home\rtest"} {
		if _, err := systemdUnit("/bin/sharedir", badHome); err == nil {
			t.Errorf("expected error for newline in home %q, got nil", badHome)
		} else if !strings.Contains(err.Error(), "путь содержит перевод строки") {
			t.Errorf("expected 'путь содержит перевод строки' error, got %v", err)
		}
	}
}
