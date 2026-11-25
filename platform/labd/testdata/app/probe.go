package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type result struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// probe tries what a broken-into app would try and reports, for each, whether
// the sandbox behaved as labd's CONTRACT.md says: "ok" means it was refused
// (or, for the allowed things, that it worked). Query: state=<labd state
// dir>, other=<another app's port>, addrs=<the server's addresses, comma
// separated>.
func probe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out := map[string]result{}
	refused := func(name string, err error) {
		if err == nil {
			out[name] = result{false, "allowed"}
		} else {
			out[name] = result{true, err.Error()}
		}
	}
	works := func(name string, err error) {
		if err != nil {
			out[name] = result{false, err.Error()}
		} else {
			out[name] = result{true, "works"}
		}
	}
	dial := func(addr string) error {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
		}
		return err
	}

	refused("connect_labd", dial("127.0.0.1:7070"))
	if other := q.Get("other"); other != "" {
		refused("connect_other_app", dial("127.0.0.1:"+other))
	}
	refused("connect_metadata", dial("169.254.169.254:80"))
	for _, a := range strings.Split(q.Get("addrs"), ",") {
		if a != "" {
			refused("connect_own_address_"+a, dial(net.JoinHostPort(a, "80")))
		}
	}
	works("connect_internet", dial("github.com:443"))
	if l, err := net.Listen("tcp", "127.0.0.1:29999"); err == nil {
		l.Close()
		refused("bind_other_port", nil)
	} else {
		refused("bind_other_port", err)
	}
	if fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); err == nil {
		syscall.Close(fd)
		refused("unix_socket", nil)
	} else {
		refused("unix_socket", err)
	}

	data := os.Getenv("DATA_DIR")
	works("write_data_dir", os.WriteFile(filepath.Join(data, "probe.txt"), []byte("x"), 0o666))
	if info, err := os.Stat(filepath.Join(data, "probe.txt")); err == nil && info.Mode().Perm() != 0o600 {
		out["private_files"] = result{false, "new files get mode " + info.Mode().Perm().String()}
	} else {
		works("private_files", err)
	}
	works("write_tmp", os.WriteFile("/tmp/probe.txt", []byte("x"), 0o600))
	if state := q.Get("state"); state != "" {
		_, err1 := os.Stat(filepath.Join(state, "state.json"))
		_, err2 := os.Stat(filepath.Join(state, "secret"))
		if err1 == nil || err2 == nil {
			refused("labd_state_hidden", nil)
		} else {
			refused("labd_state_hidden", err1)
		}
		names, _ := os.ReadDir(state)
		var list []string
		for _, n := range names {
			list = append(list, n.Name())
		}
		sort.Strings(list)
		out["labd_state_listing"] = result{len(list) == 1 && list[0] == "instances", strings.Join(list, " ")}
	}
	// Start fails only when the program can't be run at all.
	start := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		if err := cmd.Start(); err != nil {
			return err
		}
		cmd.Process.Kill()
		cmd.Wait()
		return nil
	}
	refused("exec_shell", start("/bin/sh", "-c", "true"))
	// A copy of this program in every place the app can write must not run.
	self, _ := os.ReadFile("/proc/self/exe")
	for name, dir := range map[string]string{"exec_from_data_dir": data, "exec_from_tmp": "/tmp", "exec_from_var_tmp": "/var/tmp", "exec_from_dev_shm": "/dev/shm"} {
		copyPath := filepath.Join(dir, "copy")
		if err := os.WriteFile(copyPath, self, 0o700); err != nil {
			refused(name, err) // can't even write there
			continue
		}
		refused(name, start(copyPath))
		os.Remove(copyPath)
	}
	_, err := os.Stat("/proc/1")
	refused("see_other_processes", err)
	_, err = os.Stat("/proc/sys/kernel")
	refused("see_kernel_settings", err)

	refused("io_uring", errno(syscall.Syscall(425, 1, uintptr(unsafe.Pointer(&[120]byte{})), 0)))
	refused("keyring", errno(syscall.Syscall(250, 0, ^uintptr(2), 0))) // KEYCTL_GET_KEYRING_ID, session keyring
	refused("userfaultfd", errno(syscall.Syscall(323, 1|syscall.O_CLOEXEC, 0, 0)))
	refused("user_namespace", syscall.Unshare(syscall.CLONE_NEWUSER))
	refused("setuid", errno(syscall.Syscall(syscall.SYS_SETUID, uintptr(os.Getuid()), 0, 0)))
	refused("chown", os.Chown(filepath.Join(data, "probe.txt"), os.Getuid(), os.Getgid()))
	var lim syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
	refused("setrlimit", errno(syscall.RawSyscall(syscall.SYS_SETRLIMIT, syscall.RLIMIT_NOFILE, uintptr(unsafe.Pointer(&lim)), 0)))
	buf := make([]byte, 4096)
	refused("mlock", errno(syscall.Syscall(syscall.SYS_MLOCK, uintptr(unsafe.Pointer(&buf[0])), 4096, 0)))
	if m, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC, syscall.MAP_PRIVATE|syscall.MAP_ANON); err == nil {
		syscall.Munmap(m)
		refused("write_execute_memory", nil)
	} else {
		refused("write_execute_memory", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func errno(_, _ uintptr, e syscall.Errno) error {
	if e != 0 {
		return e
	}
	return nil
}

// impostor tries to listen on ?addr= (another program's port) for ten
// seconds and answers anyone who connects, the way an app that took labd's
// port while labd restarted would.
func impostor(w http.ResponseWriter, r *http.Request) {
	l, err := net.Listen("tcp", r.URL.Query().Get("addr"))
	if err != nil {
		fmt.Fprintln(w, "refused:", err)
		return
	}
	go func() {
		time.AfterFunc(10*time.Second, func() { l.Close() })
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("impostor\n"))
			c.Close()
		}
	}()
	fmt.Fprintln(w, "listening")
}
