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
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var message_header_list = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"}

var subject_flag = flag.String("subject", "", "subject")
var release_flag = flag.Bool("release", false, "generate arXiv release")
var stdout_flag = flag.Bool("stdout", false, "print .eml directly to stdout")
var no_email_flag = flag.Bool("no-email", false, "do not generate an email, generate files instead")

type SFlags []string

var exclusions SFlags
var recipients SFlags

func (xs *SFlags) String() (str string) {
	for _, val := range *xs {
		str += fmt.Sprintf(" %s", val)
	}
	return
}

func (xs *SFlags) Set(value string) error {
	*xs = append(*xs, value)
	return nil
}

func main() {
	flag.Var(&exclusions, "x", "set exclusion prefixes (can be used multiple times)")
	flag.Var(&recipients, "to", "set To addresses (can be used multiple times)")
	flag.Parse()

	if *subject_flag == "" {
		if wd, e := os.Getwd(); e != nil {
			panic(e)
		} else {
			*subject_flag = filepath.Base(wd)
		}
	}
	if *release_flag {
		*subject_flag = *subject_flag + " [arXiv release]"
	}

	var r *git.Repository
	if re, e := git.PlainOpen("."); e != nil {
		panic(e)
	} else {
		r = re
	}

	var user_email *mail.Address
	if conf, e := r.ConfigScoped(config.GlobalScope); e != nil {
		panic(e)
	} else if addr, e := mail.ParseAddress(fmt.Sprintf("%s <%s>", conf.User.Name, conf.User.Email)); e != nil {
		panic(e)
	} else {
		user_email = addr
	}

	// use the commit to which HEAD is pointing
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

	var filelist []string
	if e := files.ForEach(func(f *object.File) error {
		if !filename_filter(f.Name, exclusions) {
			return nil
		} else {
			filelist = append(filelist, f.Name)
		}

		if r, e := f.Reader(); e != nil {
			return e
		} else {
			gname := filepath.Join(tmpdir, f.Name)
			if e := os.MkdirAll(filepath.Dir(gname), os.ModePerm); e != nil {
				return e
			} else if g, e := os.Create(gname); e != nil {
				return e
			} else if n, e := write_to_tmp_dir(r, g, f.Name, commit_hash.String()); e != nil {
				return e
			} else if *release_flag {
				// if release, do not write anything to tar writer yet
				return nil
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
				fmt.Fprintln(os.Stderr, "including:", f.Name)
			}
		}
		return nil
	}); e != nil {
		panic(e)
	}

	if e := compile_tex_code(tmpdir); e != nil {
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

	if *release_flag {
		if e := arXiv_release(tw, tmpdir, filelist, uid, gid, commit_time); e != nil {
			panic(e)
		}
	}

	cid := fmt.Sprintf("%s", commit_hash.String()[:10])

	// close the tar writer (wrapping buf)
	if e := tw.Close(); e != nil {
		panic(e)
	} else if gzbuf, e := to_gzip(buf); e != nil {
		panic(e)
	} else {
		if *no_email_flag {
			if e := generate_files(commit_object, cid, commit_time, pdfbuf, gzbuf); e != nil {
				panic(e)
			}
		} else {
			var g io.Writer
			if *stdout_flag {
				g = os.Stdout
			} else if !*no_email_flag {
				if f, e := os.Create(fmt.Sprintf("build-%s-%s.eml", cid, commit_time.Format("01-02"))); e != nil {
					panic(e)
				} else {
					g = f
					defer f.Close()
				}
			}
			if e := generate_eml(user_email, recipients, g, commit_object, cid, commit_time, pdfbuf, gzbuf); e != nil {
				panic(e)
			}
		}
	}

	if e := os.RemoveAll(tmpdir); e != nil {
		panic(e)
	}
}

func arXiv_release(tw *tar.Writer, tmpdir string, filelist []string, uid string, gid string, commit_time time.Time) error {
	log_bytes, e := os.ReadFile(filepath.Join(tmpdir, "main.log"))
	if e != nil {
		return e
	}
	filelist = append(filelist, "main.bbl")
	for _, fname := range filelist {
		if bytes.Index(log_bytes, []byte(fname)) == -1 {
			continue
		}
		if f, e := os.Open(filepath.Join(tmpdir, fname)); e != nil {
			return e
		} else if s, e := f.Stat(); e != nil {
			return e
		} else {
			hdr := &tar.Header{
				Name:    fname,
				Mode:    0644,
				Size:    s.Size(),
				Uname:   uid,
				Gname:   gid,
				ModTime: commit_time,
			}
			if e := tw.WriteHeader(hdr); e != nil {
				return e
			} else if _, e := io.Copy(tw, f); e != nil {
				return e
			}
		}
		fmt.Fprintln(os.Stderr, "including:", fname)
	}
	return nil
}

func compile_tex_code(tmpdir string) error {
	if _, e := os.Stat(filepath.Join(tmpdir, "main.tex")); e != nil {
		return fmt.Errorf("file main.tex cannot be found")
	}

	fmt.Fprintln(os.Stderr, "compiling")
	cmd := exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		return e
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
		return e
	}

	// citation command was present, run bibtex
	fmt.Fprintln(os.Stderr, "bibtex")
	cmd = exec.Command("bibtex", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		return e
	}

	fmt.Fprintln(os.Stderr, "compiling")
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		return e
	}
skip:
	fmt.Fprintln(os.Stderr, "compiling")
	cmd = exec.Command("pdflatex", "-halt-on-error", "-file-line-error", "-interaction=nonstopmode", "main")
	cmd.Dir = tmpdir
	if e := cmd.Run(); e != nil {
		return e
	}
	return nil
}

func generate_files(commit_object *object.Commit, cid string, commit_time time.Time, pdfbuf *bytes.Buffer, gzbuf io.Reader) error {
	// pdf attachment part
	var pdf_file_name string
	var targz_file_name string
	if *release_flag {
		pdf_file_name = fmt.Sprintf("release-%s-%s.pdf", cid, commit_time.Format("01-02"))
		targz_file_name = fmt.Sprintf("release-%s-%s.tar.gz", cid, commit_time.Format("01-02"))
	} else {
		pdf_file_name = fmt.Sprintf("build-%s-%s.pdf", cid, commit_time.Format("01-02"))
		targz_file_name = fmt.Sprintf("source-%s-%s.tar.gz", cid, commit_time.Format("01-02"))
	}

	if f, e := os.Create(pdf_file_name); e != nil {
		panic(e)
	} else if _, e := io.Copy(f, pdfbuf); e != nil {
		panic(e)
	} else if e := f.Close(); e != nil {
		panic(e)
	} else {
		fmt.Fprintf(os.Stderr, "writing: %s\n", pdf_file_name)
	}

	if f, e := os.Create(targz_file_name); e != nil {
		panic(e)
	} else if _, e := io.Copy(f, gzbuf); e != nil {
		panic(e)
	} else if e := f.Close(); e != nil {
		panic(e)
	} else {
		fmt.Fprintf(os.Stderr, "writing: %s\n", targz_file_name)
	}
	return nil
}

func generate_eml(user_email *mail.Address, recipients []string, g io.Writer, commit_object *object.Commit, cid string, commit_time time.Time, pdfbuf *bytes.Buffer, gzbuf io.Reader) error {
	parsed_recipients := make([]*mail.Address, 0, 4)
	if len(recipients) > 0 {
		for _, x := range recipients {
			if a, e := mail.ParseAddress(x); e != nil {
				return e
			} else {
				parsed_recipients = append(parsed_recipients, a)
			}
		}
	}

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
	message_headers["From"] = user_email.String()
	if len(parsed_recipients) > 0 {
		var collection string
		for i, x := range parsed_recipients {
			if i > 0 {
				collection = fmt.Sprintf("%s, %s", collection, x.String())
			} else {
				collection = x.String()
			}
		}
		message_headers["To"] = collection
		message_headers["Cc"] = user_email.String()
	} else {
		message_headers["To"] = user_email.String()
	}
	message_headers["Subject"] = *subject_flag
	message_headers["Date"] = time.Now().Format(time.RFC1123Z)
	message_headers["MIME-Version"] = "1.1"
	message_headers["Message-ID"] = fmt.Sprintf("%08x-%04x-%04x-%04x-%016x@liouville.net", m_id[0:4], m_id[4:6], m_id[6:8], m_id[8:10], m_id[10:18])
	message_headers["Content-Type"] = fmt.Sprintf("multipart/mixed; boundary=\"%s\"", m.Boundary())

	for k, key := range message_header_list {
		if a, ok := message_headers[key]; ok {
			if k < 4 {
				fmt.Fprintf(os.Stderr, "%s: %s\r\n", key, a)
			}
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

	if *release_flag {
		pdf_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=release-%s-%s.pdf", cid, commit_time.Format("01-02")))
	} else {
		pdf_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=build-%s-%s.pdf", cid, commit_time.Format("01-02")))
	}

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
	if *release_flag {
		targz_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=release-%s-%s.tar.gz", cid, commit_time.Format("01-02")))
	} else {
		targz_header.Add("Content-Disposition", fmt.Sprintf("attachment; filename=source-%s-%s.tar.gz", cid, commit_time.Format("01-02")))
	}
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
	return m.Close()
}

func filename_filter(s string, xs []string) bool {
	for _, f := range xs {
		if strings.HasPrefix(s, f) {
			return false
		}
	}
	return true
}

func write_to_tmp_dir(r io.ReadCloser, fi *os.File, name string, hash string) (n int64, e error) {
	switch name {
	case "main.tex":
		rb := bufio.NewReader(r)
		for {
			if l, e := rb.ReadSlice('\n'); e != nil {
				panic(e)
			} else {
				if k, er := fmt.Fprintf(fi, "%s", l); er != nil {
					return n + int64(k), er
				} else {
					n += int64(k)
				}
				if strings.Contains(fmt.Sprintf("%s", l), "documentclass") {
					if !(*release_flag) {
						// only write the atbegshi if not an arXiv release
						if k, er := fmt.Fprintf(fi, "\\usepackage{atbegshi}\n\\AtBeginShipoutNext{\\AtBeginShipoutUpperLeft{\\put(1.25in,-1in){\\makebox[0pt][l]{{\\tt %s %s}}}}}\n", hash[:8], time.Now().Format("15:04:05\\ 2006-01-02")); er != nil {
							return n + int64(k), er
						} else {
							n += int64(k)
						}
					}
					if k, er := fmt.Fprintf(fi, "%%%s\n", hash); er != nil {
						return n + int64(k), er
					} else {
						n += int64(k)
					}
					break
				}
			}
		}
		if k, er := io.Copy(fi, rb); er != nil {
			return n + k, er
		} else {
			n += k
		}
	default:
		if k, er := io.Copy(fi, r); er != nil {
			return n + k, er
		} else {
			n += k
		}
	}
	if e := r.Close(); e != nil {
		return n, e
	} else if e := fi.Close(); e != nil {
		return n, e
	} else {
		return n, nil
	}

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
