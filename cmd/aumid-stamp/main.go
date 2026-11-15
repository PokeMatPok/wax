// aumid-stamp: stamp System.AppUserModel.ID on a shortcut (.lnk) via its
// IPropertyStore. Usage: aumid-stamp.exe <path-to-lnk> <aumid>
package main

import (
	"fmt"
	"os"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	shell32   = syscall.NewLazyDLL("shell32.dll")
	procSHGet = shell32.NewProc("SHGetPropertyStoreFromParsingName")
	ole32     = syscall.NewLazyDLL("ole32.dll")
	procAlloc = ole32.NewProc("CoTaskMemAlloc")
	procCoInit = ole32.NewProc("CoInitializeEx")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func parseGUID(s string) guid {
	var g guid
	buf := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			continue
		case c >= '0' && c <= '9':
			buf = append(buf, c-'0')
		case c >= 'a' && c <= 'f':
			buf = append(buf, c-'a'+10)
		case c >= 'A' && c <= 'F':
			buf = append(buf, c-'A'+10)
		}
	}
	g.Data1 = uint32(buf[0])<<28 | uint32(buf[1])<<24 | uint32(buf[2])<<20 | uint32(buf[3])<<16 |
		uint32(buf[4])<<12 | uint32(buf[5])<<8 | uint32(buf[6])<<4 | uint32(buf[7])
	g.Data2 = uint16(buf[8])<<12 | uint16(buf[9])<<8 | uint16(buf[10])<<4 | uint16(buf[11])
	g.Data3 = uint16(buf[12])<<12 | uint16(buf[13])<<8 | uint16(buf[14])<<4 | uint16(buf[15])
	for i := 0; i < 8; i++ {
		g.Data4[i] = buf[16+i*2]<<4 | buf[17+i*2]
	}
	return g
}

var iidPropStore = parseGUID("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99") // IPropertyStore

func vt(obj uintptr) *[8]uintptr {
	return (*[8]uintptr)(unsafe.Pointer(*(*uintptr)(unsafe.Pointer(obj))))
}

func main() {
	if len(os.Args) != 3 {
		fmt.Println("usage: aumid-stamp <lnk> <aumid>")
		os.Exit(2)
	}
	path, aumid := os.Args[1], os.Args[2]

	if hr, _, _ := procCoInit.Call(0, 2); hr != 0 && hr != 1 { // COINIT_APARTMENTTHREADED; S_FALSE ok
		fmt.Printf("coinit hr=0x%08x\n", hr)
		os.Exit(1)
	}

	p, _ := syscall.UTF16PtrFromString(path)
	var ps uintptr
	hr, _, _ := procSHGet.Call(uintptr(unsafe.Pointer(p)), 0, 2, // GPS_READWRITE
		uintptr(unsafe.Pointer(&iidPropStore)), uintptr(unsafe.Pointer(&ps)))
	if hr != 0 {
		fmt.Printf("open hr=0x%08x\n", hr)
		os.Exit(1)
	}
	defer syscall.SyscallN(vt(ps)[2], ps) // Release

	// PROPERTYKEY: PKEY_AppUserModel_ID {9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3}, pid 5
	key := struct {
		fmtid guid
		pid   uint32
	}{parseGUID("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3"), 5}

	// PROPVARIANT VT_LPWSTR (31): vt at offset 0, string pointer at offset 8
	w := utf16.Encode([]rune(aumid))
	mem, _, _ := procAlloc.Call(uintptr((len(w) + 1) * 2))
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(mem)), len(w)+1)
	copy(dst, w)
	dst[len(w)] = 0
	var pv [24]byte
	*(*uint16)(unsafe.Pointer(&pv[0])) = 31
	*(*uintptr)(unsafe.Pointer(&pv[8])) = mem

	hr, _, _ = syscall.SyscallN(vt(ps)[6], ps, // SetValue
		uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&pv[0])))
	fmt.Printf("setvalue hr=0x%08x\n", hr)
	hr, _, _ = syscall.SyscallN(vt(ps)[7], ps) // Commit
	fmt.Printf("commit hr=0x%08x\n", hr)
}
