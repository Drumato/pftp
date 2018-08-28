package main

import (
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jlaffaye/ftp"
	"github.com/marcobeierer/ftps"
	"github.com/pyama86/pftp/test"
	"golang.org/x/sync/errgroup"
)

var (
	integration = flag.Bool("integration", false, "run integration tests")
)

const testCount = 2

type userInfo struct {
	ID   string
	Pass string
}

type testSet struct {
	User userInfo
	Dir  string
}

var testset = []testSet{
	testSet{userInfo{"prouser", "prouser"}, "misc/test/data/prouser"},
	testSet{userInfo{"vsuser", "vsuser"}, "misc/test/data/vsuser"},
}

const dataPath = "misc/test/data"

func localConnect(port int, t *testing.T) *ftp.ServerConn {
	client, err := ftp.Dial(fmt.Sprintf("localhost:%d", port))
	if err != nil {
		t.Fatalf("integration.localConnect() error = %v, wantErr %v", err, nil)
	}
	return client
}

func loggedin(port int, t *testing.T, user userInfo) *ftp.ServerConn {
	client := localConnect(port, t)

	err := client.Login(user.ID, user.Pass)
	if err != nil {
		t.Fatalf("integration.loggedin() error = %v, wantErr %v", err, nil)
	}
	return client
}

func TestMain(m *testing.M) {
	flag.Parse()

	srv, err := test.LaunchTestRestServer()
	if err != nil {
		fmt.Println("unable to run test webapi server")
		os.Exit(1)
	}
	defer srv.Close()

	result := m.Run()
	os.Exit(result)
}

func TestConnect(t *testing.T) {
	if !*integration {
		t.Skip()
	}
	client := localConnect(2121, t)
	defer client.Quit()
}

func TestLogin(t *testing.T) {
	if !*integration {
		t.Skip()
	}
	eg := errgroup.Group{}

	c := make(chan bool, len(testset)+1)
	for i := 0; i < len(testset); i++ {
		index := i

		c <- true
		eg.Go(func() error {
			defer func() { <-c }()

			client := localConnect(2121, t)
			defer client.Quit()

			// If Login failed with vsftpd user, Return Error
			if err := client.Login(testset[index].User.ID, testset[index].User.Pass); err != nil {
				return fmt.Errorf("integration.TestLogin() error = %v, wantErr %v", err, nil)
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestAuth(t *testing.T) {
	if !*integration {
		t.Skip()
	}
	eg := errgroup.Group{}

	c := make(chan bool, len(testset)+1)
	for i := 0; i < len(testset); i++ {
		index := i

		c <- true
		eg.Go(func() error {
			defer func() { <-c }()

			client := new(ftps.FTPS)
			defer client.Quit()
			client.Debug = true
			client.TLSConfig.InsecureSkipVerify = true

			err := client.Connect("localhost", 2121)
			if err != nil {
				return fmt.Errorf("integration.TestAuth() error = %v, wantErr %v", err, nil)
			}

			// If Login failed with vsftpd user, Return Error
			if err = client.Login(testset[index].User.ID, testset[index].User.Pass); err == nil {
				return fmt.Errorf("integration.TestAuth() error = %v, wantErr %v", err, errors.New("550 Permission denied"))
			}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

func removeDirFiles(t *testing.T, dir string) {
	for i := 0; i < len(testset); i++ {
		f := path.Join(testset[i].Dir, dir)
		filepath.Walk(f,
			func(fpath string, info os.FileInfo, err error) error {
				rel, err := filepath.Rel(f, fpath)
				if err != nil {
					t.Fatal(err)
				}
				if rel == `.` || rel == `..` {
					return nil
				}
				out, err := exec.Command("rm", "-f", fpath).CombinedOutput()
				if err != nil {
					t.Fatal(string(out))
				}
				return err
			})
	}
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func makeRandomFiles(t *testing.T) {
	eg := errgroup.Group{}
	for i := 0; i < testCount; i++ {
		num := i

		eg.Go(func() error {
			f := fmt.Sprintf("%s/%d", dataPath, num)
			if !fileExists(f) {
				// make 500MB files
				out, err := exec.Command("dd", "if=/dev/urandom", fmt.Sprintf("of=%s", f), "bs=1024", "count=500000").CombinedOutput()
				if err != nil {
					return errors.New(string(out))
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUpload(t *testing.T) {
	if !*integration {
		t.Skip()
	}
	eg := errgroup.Group{}
	userCount := len(testset)

	makeRandomFiles(t)

	removeDirFiles(t, "stor")

	c := make(chan bool, (testCount * userCount))
	for u := 0; u < userCount; u++ {
		for i := 0; i < testCount; i++ {
			c <- true
			user := u
			num := i

			eg.Go(func() error {
				defer func() { <-c }()
				a := md5.New()
				b := md5.New()

				client := loggedin(2121, t, testset[user].User)
				defer client.Quit()

				f, err := os.Open(fmt.Sprintf("%s/%d", dataPath, num))
				if err != nil {
					return err
				}
				defer f.Close()

				if err := os.MkdirAll(fmt.Sprintf("%s/stor", testset[user].Dir), 0755); err != nil {
					return err
				}

				if err = client.Stor(fmt.Sprintf("stor/%d", num), f); err != nil {
					return err
				}

				s, err := os.Open(fmt.Sprintf("%s/stor/%d", testset[user].Dir, num))
				if err != nil {
					return err
				}
				defer s.Close()

				_, err = io.Copy(a, s)
				if err != nil {
					return err
				}

				// Set file pointer to front of origin file
				_, err = f.Seek(0, 0)
				if err != nil {
					return err
				}

				_, err = io.Copy(b, f)
				if err != nil {
					return err
				}

				if !reflect.DeepEqual(a.Sum(nil), b.Sum(nil)) {
					return fmt.Errorf("upload file check sum error: %d", num)
				}
				return nil
			})
		}
	}

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestDownload(t *testing.T) {
	if !*integration {
		t.Skip()
	}
	eg := errgroup.Group{}

	removeDirFiles(t, "retr")

	userCount := len(testset)

	c := make(chan bool, (testCount * userCount))
	for u := 0; u < userCount; u++ {
		for i := 0; i < testCount; i++ {
			c <- true
			user := u
			num := i

			eg.Go(func() error {
				defer func() { <-c }()
				a := md5.New()
				b := md5.New()

				client := loggedin(2121, t, testset[user].User)
				defer client.Quit()

				r, err := client.Retr(fmt.Sprintf("stor/%d", num))
				if err != nil {
					return err
				}
				defer r.Close()

				_, err = io.Copy(a, r)
				if err != nil {
					return err
				}

				f, err := os.Open(fmt.Sprintf("%s/stor/%d", testset[user].Dir, num))
				if err != nil {
					return err
				}
				defer f.Close()

				_, err = io.Copy(b, f)
				if err != nil {
					return err
				}

				if !reflect.DeepEqual(a.Sum(nil), b.Sum(nil)) {
					return fmt.Errorf("download file check sum error: %d", num)
				}
				return nil
			})
		}
	}

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}