package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/clearsign"
	"golang.org/x/crypto/openpgp/packet"

	"github.com/ulikunitz/xz"
	"golang.org/x/crypto/openpgp"
)

type Repo struct {
	InRoot             string
	OutRoot            string
	Dists              map[string]map[string][]*Deb // packages = Dists[dist][component]
	GenerateContents   bool
	MaintainerOverride string
	Origin             string
	Description        string
	SignEntity         *openpgp.Entity
}

func NewRepo(in, out string, generateContents bool, maintainerOverride, origin, description, signPrivateKeyAsc string) (*Repo, error) {
	var err error

	if in, err = filepath.Abs(in); err != nil {
		return nil, fmt.Errorf("error resolving in path: %v", err)
	}

	if out, err = filepath.Abs(out); err != nil {
		return nil, fmt.Errorf("error resolving out path: %v", err)
	}

	if _, err := os.Stat(out); err == nil {
		return nil, errors.New("out must not exist")
	}

	block, err := armor.Decode(strings.NewReader(signPrivateKeyAsc))
	if err != nil {
		return nil, fmt.Errorf("could not load private key: could not decode armor: %v", err)
	}

	if block.Type != openpgp.PrivateKeyType {
		return nil, errors.New("could not load private key: no private key in decoded block")
	}

	pr := packet.NewReader(block.Body)
	entity, err := openpgp.ReadEntity(pr)
	if err != nil {
		return nil, fmt.Errorf("could not load private key: could not read entity: %v", err)
	}

	return &Repo{
		InRoot:             in,
		OutRoot:            out,
		Dists:              map[string]map[string][]*Deb{},
		GenerateContents:   generateContents,
		MaintainerOverride: maintainerOverride,
		Origin:             origin,
		Description:        description,
		SignEntity:         entity,
	}, nil
}

// Clean removes the out dir.
func (r *Repo) Clean() {
	os.RemoveAll(r.OutRoot)
}

// Scan scans the in dir. Layout must be in/DIST/COMPONENT/*.deb.
func (r *Repo) Scan() error {
	dists := map[string]map[string][]*Deb{}

	dfs, err := ioutil.ReadDir(r.InRoot)
	if err != nil {
		return fmt.Errorf("could not list in dir: %v", err)
	}
	for _, dfi := range dfs {
		if !dfi.IsDir() {
			return fmt.Errorf("could not scan in dir: not a dir: %s", filepath.Join(r.InRoot, dfi.Name()))
		}
		distName, distRoot := dfi.Name(), filepath.Join(r.InRoot, dfi.Name())
		dists[distName] = map[string][]*Deb{}

		cfs, err := ioutil.ReadDir(distRoot)
		if err != nil {
			return fmt.Errorf("could not list in dir subdir: %v", err)
		}
		for _, cfi := range cfs {
			if !cfi.IsDir() {
				return fmt.Errorf("could not scan in dir: not a dir: %s", filepath.Join(r.InRoot, dfi.Name(), cfi.Name()))
			}
			compName, compRoot := cfi.Name(), filepath.Join(distRoot, cfi.Name())
			dists[distName][compName] = []*Deb{}

			pfs, err := ioutil.ReadDir(compRoot)
			if err != nil {
				return fmt.Errorf("could not list in dir subdir: %v", err)
			}
			for _, pfi := range pfs {
				if pfi.IsDir() || filepath.Ext(pfi.Name()) != ".deb" {
					return fmt.Errorf("could not scan in dir: not a deb file: %s", filepath.Join(r.InRoot, dfi.Name(), cfi.Name(), pfi.Name()))
				}
				pkgFname := filepath.Join(compRoot, pfi.Name())

				d, err := NewDeb(pkgFname, r.GenerateContents)
				if err != nil {
					return fmt.Errorf("could not read deb: %v", err)
				}
				dists[distName][compName] = append(dists[distName][compName], d)
			}
		}
	}

	r.Dists = dists
	return nil
}

// MakePool copies the deb files to the pool.
func (r *Repo) MakePool() error {
	poolRoot := filepath.Join(r.OutRoot, "pool")
	if err := os.MkdirAll(poolRoot, 0755); err != nil {
		return fmt.Errorf("error making pool dir: %v", err)
	}

	for _, dist := range r.Dists {
		for compName, comp := range dist {
			compRoot := filepath.Join(poolRoot, compName)
			if err := os.MkdirAll(compRoot, 0755); err != nil {
				return fmt.Errorf("error making component dir: %v", err)
			}
			for _, d := range comp {
				pkgName := d.Control.MustGet("Package")
				pkgArch := d.Control.MustGet("Architecture")
				pkgVer := d.Control.MustGet("Version")

				letterRoot := filepath.Join(compRoot, getLetter(pkgName))
				if err := os.MkdirAll(letterRoot, 0755); err != nil {
					return fmt.Errorf("error making letter dir: %v", err)
				}

				pkgRoot := filepath.Join(letterRoot, pkgName)
				if err := os.MkdirAll(pkgRoot, 0755); err != nil {
					return fmt.Errorf("error making pkg dir: %v", err)
				}

				pkgFName := filepath.Join(pkgRoot, fmt.Sprintf("%s_%s_%s.deb", pkgName, pkgVer, pkgArch))

				f, err := os.Open(d.Filename)
				if err != nil {
					return fmt.Errorf("error opening package file for copying: %v", err)
				}

				of, err := os.Create(pkgFName)
				if err != nil {
					f.Close()
					return fmt.Errorf("error opening output package file for copying: %v", err)
				}

				_, err = io.Copy(of, f)
				if err != nil {
					of.Close()
					f.Close()
					return fmt.Errorf("error writing package file: %v", err)
				}

				of.Close()
				f.Close()
			}
		}
	}
	return nil
}

// MakeDist generates the indexes.
func (r *Repo) MakeDist() error {
	distsRoot := filepath.Join(r.OutRoot, "dists")
	if err := os.MkdirAll(distsRoot, 0755); err != nil {
		return fmt.Errorf("error making dists dir: %v", err)
	}

	for distName, dist := range r.Dists {
		distRoot := filepath.Join(distsRoot, distName)
		if err := os.MkdirAll(distRoot, 0755); err != nil {
			return fmt.Errorf("error making dist dir: %v", err)
		}
		compNames := []string{}
		archNames := []string{}
		md5Sums := []string{}
		sha256Sums := []string{}
		for compName, comp := range dist {
			compRoot := filepath.Join(distRoot, compName)
			if err := os.MkdirAll(compRoot, 0755); err != nil {
				return fmt.Errorf("error making component dir: %v", err)
			}
			archs := map[string][]*Deb{}
			for _, d := range comp {
				pkgArch := d.Control.MustGet("Architecture")
				if _, ok := archs[pkgArch]; !ok {
					archs[pkgArch] = []*Deb{}
				}
				archs[pkgArch] = append(archs[pkgArch], d)
			}
			for archName, arch := range archs {
				archRoot := filepath.Join(compRoot, "binary-"+archName)
				if err := os.MkdirAll(archRoot, 0755); err != nil {
					return fmt.Errorf("error making arch dir: %v", err)
				}
				var packages strings.Builder
				for _, d := range arch {
					c := d.Control.Clone()
					c.MoveToOrderStart("Package")
					if r.MaintainerOverride != "" {
						c.Set("Maintainer", r.MaintainerOverride)
					}
					c.Set("Size", fmt.Sprint(d.Size))
					for field, sum := range d.Sums {
						c.Set(field, sum)
					}
					c.Set("Filename", fmt.Sprintf("pool/%s/%s/%s_%s_%s.deb", compName, getLetter(c.MustGet("Package")), c.MustGet("Package"), c.MustGet("Version"), c.MustGet("Architecture")))
					packages.WriteString(c.String() + "\n")
				}
				packagesBytes := []byte(packages.String())
				md5Sums = append(md5Sums, fmt.Sprintf("%-32x % 8d %s/%s/Packages", md5sum(packagesBytes), len(packagesBytes), compName, archName))
				sha256Sums = append(sha256Sums, fmt.Sprintf("%-64x % 8d %s/%s/Packages", sha256sum(packagesBytes), len(packagesBytes), compName, archName))

				gzb := gz(packagesBytes)
				md5Sums = append(md5Sums, fmt.Sprintf("%-32x % 8d %s/%s/Packages.gz", md5sum(gzb), len(gzb), compName, archName))
				sha256Sums = append(sha256Sums, fmt.Sprintf("%-64x % 8d %s/%s/Packages.gz", sha256sum(gzb), len(gzb), compName, archName))
				err := ioutil.WriteFile(filepath.Join(archRoot, "Packages.gz"), gzb, 0644)
				if err != nil {
					return fmt.Errorf("error writing packages.gz file: %v", err)
				}

				xzb := xzip(packagesBytes)
				md5Sums = append(md5Sums, fmt.Sprintf("%-32x % 8d %s/%s/Packages.xz", md5sum(gzb), len(gzb), compName, archName))
				sha256Sums = append(sha256Sums, fmt.Sprintf("%-64x % 8d %s/%s/Packages.xz", sha256sum(gzb), len(gzb), compName, archName))
				err = ioutil.WriteFile(filepath.Join(archRoot, "Packages.xz"), xzb, 0644)
				if err != nil {
					return fmt.Errorf("error writing packages.xz file: %v", err)
				}

				archNames = append(archNames, archName)
			}

			if r.GenerateContents {
				for archName, arch := range archs {
					var b strings.Builder
					contents := map[string][]string{}
					for _, d := range arch {
						for _, fn := range d.Contents {
							if _, ok := contents[fn]; !ok {
								contents[fn] = []string{}
							}
							qname := d.Control.MustGet("Package") // qname is the qualified package name [$SECTION/]$NAME
							if s, ok := d.Control.Get("Section"); ok {
								qname = s + "/" + qname
							}
							contents[fn] = append(contents[fn], qname)
						}
					}

					fns := []string{}
					for fn := range contents {
						fns = append(fns, fn)
					}
					sort.Strings(fns)

					for _, fn := range fns {
						b.WriteString(fmt.Sprintf("%-56s %s\n", fn, strings.Join(contents[fn], ",")))
					}

					contentsBytes := []byte(b.String())
					md5Sums = append(md5Sums, fmt.Sprintf("%-32x % 8d %s/Contents-%s", md5sum(contentsBytes), len(contentsBytes), compName, archName))
					sha256Sums = append(sha256Sums, fmt.Sprintf("%-64x % 8d %s/Contents-%s", sha256sum(contentsBytes), len(contentsBytes), compName, archName))

					gzb := gz(contentsBytes)
					md5Sums = append(md5Sums, fmt.Sprintf("%-32x % 8d %s/Contents-%s.gz", md5sum(gzb), len(gzb), compName, archName))
					sha256Sums = append(sha256Sums, fmt.Sprintf("%-64x % 8d %s/Contents-%s.gz", sha256sum(gzb), len(gzb), compName, archName))
					err := ioutil.WriteFile(filepath.Join(compRoot, "Contents-"+archName+".gz"), gzb, 0644)
					if err != nil {
						return fmt.Errorf("error writing contents-"+archName+".gz file: %v", err)
					}
				}
			}
			compNames = append(compNames, compName)
		}
		release := NewControl()
		if r.Origin != "" {
			release.Set("Origin", r.Origin)
		}
		release.Set("Suite", distName)
		release.Set("Codename", distName)
		release.Set("Date", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 MST"))
		release.Set("Components", strings.Join(compNames, " "))
		release.Set("Architectures", strings.Join(archNames, " "))
		release.Set("Description", r.Description)
		release.Set("MD5Sum", "\n"+strings.Join(md5Sums, "\n"))
		release.Set("SHA256", "\n"+strings.Join(sha256Sums, "\n"))
		err := ioutil.WriteFile(filepath.Join(distRoot, "Release"), []byte(release.String()), 0644)
		if err != nil {
			return fmt.Errorf("error writing release file: %v", err)
		}

		releasegpg := new(bytes.Buffer)
		err = openpgp.ArmoredDetachSign(releasegpg, r.SignEntity, strings.NewReader(release.String()), nil)
		if err != nil {
			return fmt.Errorf("error signing release file: %v", err)
		}
		err = ioutil.WriteFile(filepath.Join(distRoot, "Release.gpg"), releasegpg.Bytes(), 0644)
		if err != nil {
			return fmt.Errorf("error writing release.gpg file: %v", err)
		}

		inrelease := new(bytes.Buffer)
		dec, err := clearsign.Encode(inrelease, r.SignEntity.PrivateKey, nil)
		if err != nil {
			return fmt.Errorf("error clearsigning release file: %v", err)
		}
		if _, err := io.WriteString(dec, release.String()); err != nil {
			return fmt.Errorf("error clearsigning release file: %v", err)
		}
		dec.Close()
		err = ioutil.WriteFile(filepath.Join(distRoot, "InRelease"), inrelease.Bytes(), 0644)
		if err != nil {
			return fmt.Errorf("error writing inrelease file: %v", err)
		}
	}
	return nil
}

func getLetter(pkg string) string {
	if strings.HasPrefix(pkg, "lib") {
		return pkg[:4]
	}
	return pkg[:1]
}

func md5sum(data []byte) []byte {
	s := md5.New()
	if _, err := s.Write(data); err != nil {
		panic(err)
	}
	return s.Sum(nil)
}

func sha256sum(data []byte) []byte {
	s := sha256.New()
	if _, err := s.Write(data); err != nil {
		panic(err)
	}
	return s.Sum(nil)
}

func gz(data []byte) []byte {
	b := new(bytes.Buffer)
	w := gzip.NewWriter(b)
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	w.Close()
	return b.Bytes()
}

func xzip(data []byte) []byte {
	b := new(bytes.Buffer)
	w, err := xz.NewWriter(b)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	w.Close()
	return b.Bytes()
}