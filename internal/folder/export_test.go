package folder

// SetWaitEntered installs a barrier before starting any concurrent requests.
func SetWaitEntered(f *Folder, hook func()) { f.waitEntered = hook }
