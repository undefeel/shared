package i18n

import "testing"

func TestT(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if got := T("synced", 0); got != "synced" {
		t.Errorf("en synced = %q", got)
	}
	if got := T("peer_paused"); got != "peer paused" {
		t.Errorf("en peer_paused = %q", got)
	}
	if got := T("retry", 3); got != "failed: 3, retrying" {
		t.Errorf("en retry = %q", got)
	}
	t.Setenv("LANG", "ru_RU.UTF-8")
	if got := T("retry", 3); got != "не удалось: 3, повтор" {
		t.Errorf("ru retry = %q", got)
	}
	if got := T("Paired devices:"); got != "Сопряжённые устройства:" {
		t.Errorf("ru = %q", got)
	}
	t.Setenv("LC_ALL", "C")
	if Lang() != "en" {
		t.Error("LC_ALL должен перекрывать LANG")
	}
}
