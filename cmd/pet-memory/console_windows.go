//go:build windows

package main

import "syscall"

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	getConsoleOutputCP = kernel32.NewProc("GetConsoleOutputCP")
	setConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
)

// useUTF8Console 把控制台输出代码页切到 UTF-8。
// Go 的 os.Stdout 只写 UTF-8 字节，而 Windows 控制台默认按本地代码页（中文系统是 936/GBK）
// 解释这些字节，中文因此变成乱码；切到 65001 后字节原样透传即可正确显示。
// 返回的函数用于还原原代码页，否则同一窗口里后续按本地代码页输出的程序会反过来乱码。
// 非控制台（重定向到文件/管道）时 GetConsoleOutputCP 仍返回代码页，但不影响已写出的 UTF-8 字节。
func useUTF8Console() (restore func()) {
	noop := func() {}
	old, _, _ := getConsoleOutputCP.Call()
	if old == 0 || old == 65001 {
		return noop
	}
	if ok, _, _ := setConsoleOutputCP.Call(65001); ok == 0 {
		return noop
	}
	return func() { setConsoleOutputCP.Call(old) }
}
