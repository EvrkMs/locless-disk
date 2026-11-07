package main

import (
	"fmt"
	"image"
	"os/exec"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// The window uses only old user32/gdi32 calls: WinPE has no desktop shell and
// usually no video driver, but these still work there.
var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procBeginPaint          = user32.NewProc("BeginPaint")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procEndPaint            = user32.NewProc("EndPaint")
	procGetDC               = user32.NewProc("GetDC")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
	procSetTimer            = user32.NewProc("SetTimer")
	procShowCursor          = user32.NewProc("ShowCursor")
	procShowWindow          = user32.NewProc("ShowWindow")
	procGetKeyState         = user32.NewProc("GetKeyState")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procSetDIBitsToDevice   = gdi32.NewProc("SetDIBitsToDevice")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

const (
	wsPopup     = 0x80000000
	wsVisible   = 0x10000000
	wsExTopmost = 0x00000008

	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmPaint      = 0x000f
	wmEraseBkgnd = 0x0014
	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104
	wmTimer      = 0x0113

	vkControl = 0x11
	vkMenu    = 0x12 // Alt
	vkDelete  = 0x2e

	smCxScreen = 0
	smCyScreen = 1

	swHide = 0
	swShow = 5

	createNewConsole = 0x00000010

	frameInterval = 66 // milliseconds, about 15 frames per second
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type message struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	private uint32
}

type paintStruct struct {
	hdc       uintptr
	erase     int32
	paint     [4]int32
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

type bitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

// window is the running window. Win32 calls back into a plain function, so it
// lives in a package variable; there is only ever one.
var window struct {
	hwnd  atomic.Uintptr
	paint func(*image.RGBA)
	key   func(int) bool
	frame *image.RGBA
	bgra  []byte
	info  bitmapInfoHeader
}

// closeWindow ends runWindow. It is safe from any goroutine.
func closeWindow() {
	procPostMessageW.Call(window.hwnd.Load(), wmClose, 0, 0)
}

// openShell hides the screen and opens a command prompt for diagnostics. The
// screen comes back when the prompt is closed. It is safe from any goroutine.
func openShell() {
	hwnd := window.hwnd.Load()
	procShowWindow.Call(hwnd, swHide)
	defer func() {
		procShowWindow.Call(hwnd, swShow)
		procSetForegroundWindow.Call(hwnd)
	}()
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	cmd.Run()
}

// runWindow opens a borderless window over the whole screen and blocks until
// key asks to quit. paint draws a frame; both run on the calling thread.
func runWindow(paint func(*image.RGBA), key func(int) bool) error {
	// Without this a scaled display would stretch the frame and blur it.
	if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
	width, _, _ := procGetSystemMetrics.Call(smCxScreen)
	height, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if width == 0 || height == 0 {
		return fmt.Errorf("no screen: %dx%d", width, height)
	}

	window.paint, window.key = paint, key
	window.frame = image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	window.bgra = make([]byte, len(window.frame.Pix))
	window.info = bitmapInfoHeader{
		width:    int32(width),
		height:   -int32(height), // negative: rows go top to bottom, like image.RGBA
		planes:   1,
		bitCount: 32,
	}
	window.info.size = uint32(unsafe.Sizeof(window.info))

	instance, _, _ := procGetModuleHandleW.Call(0)
	name, err := syscall.UTF16PtrFromString("FOGsAgent")
	if err != nil {
		return err
	}
	class := wndClassEx{
		wndProc:   syscall.NewCallback(windowProc),
		instance:  instance,
		className: name,
	}
	class.size = uint32(unsafe.Sizeof(class))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); r == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}
	hwnd, _, err := procCreateWindowExW.Call(
		wsExTopmost, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)),
		wsPopup|wsVisible, 0, 0, width, height, 0, 0, instance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	window.hwnd.Store(hwnd)
	procSetForegroundWindow.Call(hwnd)
	procShowCursor.Call(0)
	if r, _, err := procSetTimer.Call(hwnd, 1, frameInterval, 0); r == 0 {
		return fmt.Errorf("SetTimer: %w", err)
	}

	var m message
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("GetMessage: %w", err)
		case 0:
			return nil
		}
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// keyHeld reports whether a key is down right now: GetKeyState sets the high bit.
func keyHeld(vk uintptr) bool {
	r, _, _ := procGetKeyState.Call(vk)
	return int16(r) < 0
}

// windowProc handles the few messages the screen cares about.
func windowProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTimer:
		timed(&uiStats.renderNs, func() { window.paint(window.frame) })
		hdc, _, _ := procGetDC.Call(hwnd)
		timed(&uiStats.presentNs, func() { present(hdc) })
		procReleaseDC.Call(hwnd, hdc)
		uiStats.frames.Add(1)
		return 0
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		timed(&uiStats.presentNs, func() { present(hdc) })
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmEraseBkgnd:
		return 1 // every frame covers the whole window
	case wmKeyDown, wmSysKeyDown: // keys pressed with Alt arrive as system keys
		code := int(wParam)
		if code == vkDelete && keyHeld(vkControl) && keyHeld(vkMenu) {
			code = keyCtrlAltDel
		}
		if window.key(code) {
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// present copies the current frame to the window. GDI wants blue first.
func present(hdc uintptr) {
	src, dst := window.frame.Pix, window.bgra
	for i := 0; i+3 < len(src); i += 4 {
		dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 0xff
	}
	b := window.frame.Bounds()
	procSetDIBitsToDevice.Call(
		hdc, 0, 0, uintptr(b.Dx()), uintptr(b.Dy()), 0, 0, 0, uintptr(b.Dy()),
		uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(&window.info)), 0,
	)
}
