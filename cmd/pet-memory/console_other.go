//go:build !windows

package main

// useUTF8Console 在非 Windows 平台无事可做：终端本来就按 UTF-8 解释输出。
func useUTF8Console() (restore func()) { return func() {} }
