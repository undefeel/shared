package folder

import (
	"github.com/fsnotify/fsevents"
	"testing"
)

func TestDarwinDroppedHistoryRequiresFull(t *testing.T) {
	for _, flag := range []fsevents.EventFlags{fsevents.MustScanSubDirs, fsevents.UserDropped, fsevents.KernelDropped, fsevents.EventIDsWrapped} {
		if !darwinWatchLoss(flag) {
			t.Fatalf("loss flag %v ignored", flag)
		}
	}
	if darwinWatchLoss(fsevents.ItemModified) {
		t.Fatal("ordinary edit classified as history loss")
	}
}
