package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/textproto"

	"mime/multipart"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"

	"github.com/go-git/go-git/v5/plumbing/object"
)

var message_header_list = []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"}

var subject_flag = flag.String("s", "", "subject")
var repository_flag = flag.String("r", ".", "repository")

func main() {
	flag.Parse()
	if *subject_flag == "" {
		if wd, e := os.Getwd(); e != nil {
			panic(e)
		} else {
			*subject_flag = filepath.Base(wd)
		}
	}

	var r *git.Repository
	if re, e := git.PlainOpen(*repository_flag); e != nil {
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
	gzbuf, e := to_gzip(buf)
	if e != nil {
		panic(e)
	}

	if _, e := os.Stat(filepath.Join(tmpdir, "main.tex")); e != nil {
		panic("file main.tex cannot be found")
	}

	fmt.Println("compiling")
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

	fmt.Println("bibtex")
	cmd = exec.Command("bibtex", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}

	fmt.Println("compiling")
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}
skip:
	fmt.Println("compiling")
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		panic(e)
	}

	// end of compiling document

	pdfbuf := bytes.NewBuffer(nil)

	if f, e := os.Open(filepath.Join(tmpdir, "main.pdf")); e != nil {
		panic(e)
	} else if _, e := io.Copy(pdfbuf, f); e != nil {
		panic(e)
	} else if e := f.Close(); e != nil {
		panic(e)
	}

	g, _ := os.Create(fmt.Sprintf("build-%s-%s.eml", cid, commit_time.Format("01-02")))
	defer g.Close()
	m := multipart.NewWriter(g)
	randy := rand.Reader
	m_id := make([]byte, 18)
	time_unix := time.Now().Unix()
	for i := 0; i < 4; i++ {
		m_id[i] = byte(time_unix)
		time_unix = time_unix / 256
	}
	randy.Read(m_id[4:])

	message_headers := make(map[string]string)
	message_headers["From"] = "barkyq-git-bot <barkyq-git-bot@liouville.net>"
	message_headers["To"] = "barkyq-git-bot <barkyq-git-bot@liouville.net>"
	message_headers["Subject"] = *subject_flag
	message_headers["Date"] = time.Now().Format(time.RFC1123Z)
	message_headers["MIME-Version"] = "1.1"
	message_headers["Message-ID"] = fmt.Sprintf("%08x-%04x-%04x-%04x-%016x@liouville.net", m_id[0:4], m_id[4:6], m_id[6:8], m_id[8:10], m_id[10:18])
	message_headers["Content-Type"] = fmt.Sprintf("multipart/mixed; boundary=\"%s\"", m.Boundary())

	for _, key := range message_header_list {
		if a, ok := message_headers[key]; ok {
			fmt.Fprintf(g, "%s: %s\r\n", key, a)
		}
	}
	g.Write([]byte{'\r', '\n'})

	// text part
	text_header := make(textproto.MIMEHeader)
	text_header.Add("Content-Type", "text/plain")
	if w, e := m.CreatePart(text_header); e != nil {
		panic(e)
	} else {
		fmt.Fprintf(w, "%s\n%s\n\n%s\n", commit_object.Author.String(), commit_object.Author.When.Format("Mon Jan 02 15:04:05 2006 -0700"), commit_object.Message)
	}

	// pdf attachment part
	pdf_header := make(textproto.MIMEHeader)
	pdf_header.Add("Content-Transfer-Encoding", "base64")
	pdf_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=build-%s-%s.pdf", cid, commit_time.Format("01-02")))
	pdf_header.Add("Content-Type", "application/pdf")
	if w, e := m.CreatePart(pdf_header); e != nil {
		panic(e)
	} else {
		rp, wp := io.Pipe()
		bw := base64.NewEncoder(base64.StdEncoding, wp)
		go func() {
			if _, e := io.Copy(bw, pdfbuf); e != nil {
				panic(e)
			} else if e := bw.Close(); e != nil {
				panic(e)
			} else if e := wp.Close(); e != nil {
				panic(e)
			}
		}()
		buffer := make([]byte, 76)
		for {
			n, _ := rp.Read(buffer)
			w.Write(buffer[:n])
			if n < 76 {
				k, e := rp.Read(buffer[n:])
				if e != nil {
					break
				}
				w.Write(buffer[n : n+k])
			}
			w.Write([]byte{'\r', '\n'})
		}
		w.Write([]byte{'\r', '\n'})
	}

	// tar attachment part
	targz_header := make(textproto.MIMEHeader)
	targz_header.Add("Content-Transfer-Encoding", "base64")
	targz_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=source-%s-%s.tar.gz", cid, commit_time.Format("01-02")))
	targz_header.Add("Content-Type", "application/gzip")
	if w, e := m.CreatePart(targz_header); e != nil {
		panic(e)
	} else {
		rp, wp := io.Pipe()
		bw := base64.NewEncoder(base64.StdEncoding, wp)
		go func() {
			if _, e := io.Copy(bw, gzbuf); e != nil {
				panic(e)
			} else if e := bw.Close(); e != nil {
				panic(e)
			} else if e := wp.Close(); e != nil {
				panic(e)
			}
		}()
		buffer := make([]byte, 76)
		for {
			n, _ := rp.Read(buffer)
			w.Write(buffer[:n])
			if n < 76 {
				k, e := rp.Read(buffer[n:])
				if e != nil {
					break
				}
				w.Write(buffer[n : n+k])
			}
			w.Write([]byte{'\r', '\n'})
		}
		w.Write([]byte{'\r', '\n'})
	}
	if e := m.Close(); e != nil {
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
