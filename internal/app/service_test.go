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
	for _, tc := range []struct{ exe, home, exec, env string }{
		{"/путь с пробелом/sharedir", "/home/путь с пробелом", `ExecStart="/путь с пробелом/sharedir" run`, `Environment="SHAREDIR_HOME=/home/путь с пробелом"`},
		{`/path "with quotes" and \slash and %percent/sharedir`, `/home "user" %dir`, `ExecStart="/path \"with quotes\" and \\slash and %%percent/sharedir" run`, `Environment="SHAREDIR_HOME=/home \"user\" %%dir"`},
	} {
		unit, err := systemdUnit(tc.exe, tc.home)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{tc.exec, tc.env} {
			if !strings.Contains(unit, want) {
				t.Errorf("expected unit to contain %q, got:\n%s", want, unit)
			}
		}
	}
	for _, bad := range []string{"/path\nwith/newline", "/path\rwith/return"} {
		for _, tc := range []struct{ exe, home string }{{bad, "/home/test"}, {"/bin/sharedir", bad}} {
			if _, err := systemdUnit(tc.exe, tc.home); err == nil || !strings.Contains(err.Error(), "путь содержит перевод строки") {
				t.Errorf("expected newline error for %q %q, got %v", tc.exe, tc.home, err)
			}
		}
	}
}
