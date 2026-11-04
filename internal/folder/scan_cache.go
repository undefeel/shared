package folder

import (
	"os"
	"reflect"
)

type scanStat struct {
	dev, ino     uint64
	size         int64
	mtime, ctime int64
	mode         uint32
}

type scanHashCache struct {
	scanStat
	hash string
}

func statNumber(v reflect.Value) int64 {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint())
	}
	return 0
}

func statField(v reflect.Value, names ...string) reflect.Value {
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	for _, name := range names {
		if f := v.FieldByName(name); f.IsValid() {
			return f
		}
	}
	return reflect.Value{}
}

func statTime(v reflect.Value, names ...string) int64 {
	t := statField(v, names...)
	sec := statNumber(statField(t, "Sec", "Tv_sec"))
	nsec := statNumber(statField(t, "Nsec", "Nsec", "Tv_nsec"))
	return sec*1e9 + nsec
}

func scanStatFromSys(sys any, size int64, mode uint32) scanStat {
	v := reflect.ValueOf(sys)
	return scanStat{
		dev:   uint64(statNumber(statField(v, "Dev"))),
		ino:   uint64(statNumber(statField(v, "Ino"))),
		size:  size,
		mtime: statTime(v, "Mtim", "Mtimespec"),
		ctime: statTime(v, "Ctim", "Ctimespec"),
		mode:  mode,
	}
}

func scanStatFromInfo(info os.FileInfo) scanStat {
	v := reflect.ValueOf(info.Sys())
	mode := uint32(statNumber(statField(v, "Mode")))
	return scanStatFromSys(info.Sys(), info.Size(), mode)
}
