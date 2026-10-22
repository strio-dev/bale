// Package wasmtest builds tiny, self-contained WASI command fixtures for tests.
package wasmtest

// Command writes output to stdout and calls WASI proc_exit with exit.
// The guest deliberately ignores fd_write errors so hosts must enforce sticky
// output limits even when the guest exits successfully.
func Command(output string, exit uint32) []byte {
	return command(output, exit, 1)
}

// Stderr writes output to stderr and calls WASI proc_exit with exit.
func Stderr(output string, exit uint32) []byte {
	return command(output, exit, 2)
}

func command(output string, exit, descriptor uint32) []byte {
	module := []byte("\x00asm\x01\x00\x00\x00")
	module = section(module, 1, []byte{3, 0x60, 4, 0x7f, 0x7f, 0x7f, 0x7f, 1, 0x7f, 0x60, 1, 0x7f, 0, 0x60, 0, 0})
	imports := []byte{2}
	imports = append(imports, name("wasi_snapshot_preview1")...)
	imports = append(imports, name("fd_write")...)
	imports = append(imports, 0, 0)
	imports = append(imports, name("wasi_snapshot_preview1")...)
	imports = append(imports, name("proc_exit")...)
	imports = append(imports, 0, 1)
	module = section(module, 2, imports)
	module = section(module, 3, []byte{1, 2})
	pages := uint32((len(output) + 16 + 65535) / 65536)
	module = section(module, 5, append([]byte{1, 0}, uintLEB(pages)...))
	exports := append([]byte{2}, name("memory")...)
	exports = append(exports, 2, 0)
	exports = append(exports, name("_start")...)
	exports = append(exports, 0, 2)
	module = section(module, 7, exports)
	body := []byte{0, 0x41, 0, 0x41, 16, 0x36, 2, 0, 0x41, 4, 0x41}
	body = append(body, intLEB(int64(len(output)))...)
	body = append(body, 0x36, 2, 0, 0x41, byte(descriptor), 0x41, 0, 0x41, 1, 0x41, 8, 0x10, 0, 0x1a, 0x41)
	body = append(body, intLEB(int64(int32(exit)))...)
	body = append(body, 0x10, 1, 0x0b)
	code := append([]byte{1}, uintLEB(uint32(len(body)))...)
	module = section(module, 10, append(code, body...))
	data := []byte{1, 0, 0x41, 16, 0x0b}
	data = append(data, name(output)...)
	return section(module, 11, data)
}

// Loop runs forever without making host calls, exercising engine cancellation.
func Loop() []byte {
	return []byte{0, 97, 115, 109, 1, 0, 0, 0, 1, 4, 1, 96, 0, 0, 3, 2, 1, 0, 7, 10, 1, 6, 95, 115, 116, 97, 114, 116, 0, 0, 10, 9, 1, 7, 0, 3, 64, 12, 0, 11, 11}
}

func section(module []byte, id byte, data []byte) []byte {
	module = append(module, id)
	module = append(module, uintLEB(uint32(len(data)))...)
	return append(module, data...)
}
func name(value string) []byte { return append(uintLEB(uint32(len(value))), value...) }
func uintLEB(value uint32) []byte {
	var result []byte
	for {
		next := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			next |= 0x80
		}
		result = append(result, next)
		if value == 0 {
			return result
		}
	}
}
func intLEB(value int64) []byte {
	var result []byte
	for {
		next := byte(value & 0x7f)
		value >>= 7
		done := (value == 0 && next&0x40 == 0) || (value == -1 && next&0x40 != 0)
		if !done {
			next |= 0x80
		}
		result = append(result, next)
		if done {
			return result
		}
	}
}
