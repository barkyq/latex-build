package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"

	// "github.com/go-git/go-git/v5/plumbing"
	// "github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func main() {
	var r *git.Repository
	if re, e := git.PlainOpen("~/projects/chord_conjecture/"); e != nil {
		panic(e)
	} else {
		r = re
	}
	var commit_object *object.Commit
	if ref, e := r.Head(); e == nil {
		if c, e := r.CommitObject(ref.Hash()); e != nil {
			panic(fmt.Sprintf("invalid commit hash: %s", e))
		} else {
			commit_object = c
		}
	} else {
		panic(e)
	}

	commit_hash := commit_object.Hash
	commit_time := commit_object.Author.When
	tr, e := commit_object.Tree()
	if e != nil {
		panic(e)
	}
	files := tr.Files()
	// hasher := sha256.New()

	buf := bytes.NewBuffer(nil)
	tw := tar.NewWriter(buf)

	var uid, gid string
	if u, e := user.Current(); e != nil {
		panic(e)
	} else {
		uid, gid = u.Uid, u.Gid
	}

	var tmpdir string
	if t, e := os.MkdirTemp("", "texdir"); e != nil {
		panic(e)
	} else {
		tmpdir = t
	}

	if e := files.ForEach(func(f *object.File) error {
		if !filename_filter(f.Name) {
			return nil
		}
		if r, e := f.Reader(); e != nil {
			return e
		} else {
			gname := filepath.Join(tmpdir, f.Name)
			if g, e := os.Create(gname); e != nil {
				return e
			} else if n, e := io.Copy(g, r); e != nil {
				return e
			} else if e := r.Close(); e != nil {
				return e
			} else if e := g.Close(); e != nil {
				return e
			} else if g, e := os.Open(g.Name()); e != nil {
				return e
			} else {
				hdr := &tar.Header{
					Name:    f.Name,
					Mode:    0644,
					Size:    n,
					Uname:   uid,
					Gname:   gid,
					ModTime: commit_time,
				}
				if e := tw.WriteHeader(hdr); e != nil {
					return e
				} else if _, e := io.Copy(tw, g); e != nil {
					return e
				}
			}
		}
		return nil
	}); e != nil {
		panic(e)
	}
	if e := tw.Close(); e != nil {
		panic(e)
	}

	cid := fmt.Sprintf("%s", commit_hash.String()[:10])
	if gzbuf, e := to_gzip(buf); e != nil {
		panic(e)
	} else if f, e := os.Create(fmt.Sprintf("source-%s-%s.tar.gz", cid, commit_time.Format("01-02"))); e == nil {
		io.Copy(f, gzbuf)
	}

	cmd := exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}
	if f, e := os.Open(filepath.Join(tmpdir, "main.aux")); e == nil {
		b := bufio.NewReader(f)
		for {
			if l, e := b.ReadString('\n'); e != nil {
				f.Close()
				goto skip
			} else if strings.HasPrefix(l, "\\citation") {
				// has citation
				f.Close()
				break
			}
		}
	} else {
		f.Close()
		panic(e)
	}

	// citation command was present, run bibtex
	cmd = exec.Command("bibtex", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}
skip:
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}
	if f, e := os.Open(filepath.Join(tmpdir, "main.pdf")); e != nil {
		panic(e)
	} else if g, e := os.Create(fmt.Sprintf("build-%s-%s.pdf", cid, commit_time.Format("01-02"))); e != nil {
		panic(e)
	} else if _, e := io.Copy(g, f); e != nil {
		panic(e)
	}
	if e := os.RemoveAll(tmpdir); e != nil {
		panic(e)
	}
}

var filenames = []string{"main.tex", "citations.bib"}

func filename_filter(s string) bool {
	for _, f := range filenames {
		if s == f {
			return true
		}
	}
	return false
}

func to_gzip(r io.Reader) (io.Reader, error) {
	buf := bytes.NewBuffer(nil)
	w := gzip.NewWriter(buf)
	if _, e := io.Copy(w, r); e != nil {
		return nil, e
	} else if e := w.Close(); e != nil {
		return nil, e
	} else {
		return buf, nil
	}
}
