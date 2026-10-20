package cli

import (
	"path"

	libfossil "github.com/danmestas/go-libfossil"
)

// repoPaths matches a PATH argument against a repository's file names as
// fossil does. fossil normalizes the argument (file_tree_name: no leading
// "./", trailing or doubled "/", "/./" or "/x/../") and compares it with
// strcmp when the case-sensitive setting is on, and with fossil_stricmp,
// which folds ASCII letters only, when it is off. The setting is the
// repository's, else fossil's platform default: off on macOS and Windows.
type repoPaths struct {
	foldCase bool
}

func newRepoPaths(r *libfossil.Repo) repoPaths {
	return repoPaths{foldCase: !r.Inner().FilenamesCaseSensitive()}
}

// clean is name as fossil's file_tree_name leaves it for a path relative to
// the repository root. An empty name stays empty.
func (p repoPaths) clean(name string) string {
	if name == "" {
		return ""
	}
	return path.Clean(name)
}

// equal reports whether the file names a and b are the same file.
func (p repoPaths) equal(a, b string) bool {
	if !p.foldCase {
		return a == b
	}
	return equalASCIIFold(a, b)
}

// within reports whether file is the file named name, or a file under the
// directory named name, as fossil's file_dir_match does. The name "." is
// every file.
func (p repoPaths) within(file, name string) bool {
	if name == "." {
		return true
	}
	if p.equal(file, name) {
		return true
	}
	return len(file) > len(name) && file[len(name)] == '/' && p.equal(file[:len(name)], name)
}

// equalASCIIFold is fossil_stricmp(a, b) == 0: a and b are equal once ASCII
// upper case letters are folded to lower case. Other bytes compare exactly,
// where strings.EqualFold would also fold Unicode.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// fileIn is the name by which version rid holds the file name asks for. With
// case folding that is the first file, in manifest order, that equals name,
// as fossil's manifest_file_find returns it, and "" when there is none;
// without it, name itself, cleaned, whether or not the version holds it.
func (p repoPaths) fileIn(r *libfossil.Repo, rid int64, name string) (string, error) {
	name = p.clean(name)
	if !p.foldCase {
		return name, nil
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if p.equal(f.Name, name) {
			return f.Name, nil
		}
	}
	return "", nil
}
