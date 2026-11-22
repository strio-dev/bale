package transport

import "os"

func openLayoutFiles(path string) (layoutFiles, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return layoutFiles{}, err
	}
	return layoutFiles{root: root}, nil
}

func (f layoutFiles) openLayoutFile(name string) (*os.File, error) {
	return f.root.Open(name)
}
