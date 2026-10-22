package console

// Before it was named Rymesh the program was "agentconsole". An install made
// then is moved to the new names the first time the new program manages it:
// directories are renamed and a symlink is left at each old path (agent tasks
// started earlier keep writing through absolute paths), paths inside
// config.json follow, the database file is renamed, and the old service
// registrations are replaced by the new ones.

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	legacyName         = "agentconsole"
	legacyUnit         = "agentconsole"      // `agentconsole install` (system service)
	legacyNodeUnit     = "agentconsole-node" // `agentconsole node setup`
	legacyLaunchdLabel = "com.agentconsole.node"
	legacyConfigPath   = "/etc/agentconsole/config.json"
)

// legacySystemDirs pairs the old system-install directories with the new.
var legacySystemDirs = [][2]string{
	{"/etc/agentconsole", "/etc/rymesh"},
	{"/var/lib/agentconsole", "/var/lib/rymesh"},
	{"/var/log/agentconsole", "/var/log/rymesh"},
}

// systemConfigPath is the default config of a system install: the new path,
// or the old one while an install has not been migrated yet.
func systemConfigPath() string {
	if !fileExists(defaultConfigPath) && fileExists(legacyConfigPath) {
		return legacyConfigPath
	}
	return defaultConfigPath
}

func isRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
}

// moveLegacyDir also handles a destination created by the SSH uploader. Check
// the entire tree for conflicts before moving anything; never overwrite data.
func moveLegacyDir(old, new string) (bool, error) {
	if !isRealDir(old) {
		return false, nil
	}
	if _, err := os.Lstat(new); err == nil {
		if err := checkLegacyMerge(old, new); err != nil {
			return false, err
		}
		if err := mergeLegacyDir(old, new); err != nil {
			return false, err
		}
		return true, os.Symlink(new, old)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(new), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(old, new); err != nil {
		return false, err
	}
	return true, os.Symlink(new, old)
}

func checkLegacyMerge(old, new string) error {
	fi, err := os.Lstat(new)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("迁移目标已存在，保留两份数据并停止：%s", new)
	}
	entries, err := os.ReadDir(old)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src, dst := filepath.Join(old, e.Name()), filepath.Join(new, e.Name())
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if !e.IsDir() {
			return fmt.Errorf("迁移文件冲突，保留两份数据并停止：%s", dst)
		}
		if err := checkLegacyMerge(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func mergeLegacyDir(old, new string) error {
	entries, err := os.ReadDir(old)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src, dst := filepath.Join(old, e.Name()), filepath.Join(new, e.Name())
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err := mergeLegacyDir(src, dst); err != nil {
			return err
		}
	}
	return os.Remove(old) // Empty now; never remove a tree containing data.
}

// renameLegacyPaths moves the config's own directories to their new names.
func (c *Config) renameLegacyPaths(pairs [][2]string) bool {
	changed := false
	fix := func(p *string) {
		for _, pr := range pairs {
			if *p == pr[0] || strings.HasPrefix(*p, pr[0]+"/") {
				*p = pr[1] + strings.TrimPrefix(*p, pr[0])
				changed = true
				return
			}
		}
	}
	fix(&c.DataDir)
	fix(&c.LogDir)
	return changed
}

// migrateLegacyFiles renames agentconsole.db and agentconsole.log. The
// database is checkpointed first so its WAL holds nothing when the files
// move. Only call it when no process has the database open.
func migrateLegacyFiles(dataDir, logDir string) error {
	oldDB, newDB := filepath.Join(dataDir, legacyName+".db"), filepath.Join(dataDir, "rymesh.db")
	if fileExists(oldDB) && !fileExists(newDB) {
		db, err := sql.Open("sqlite", "file:"+oldDB+"?_pragma=busy_timeout(8000)")
		if err != nil {
			return err
		}
		var busy, logFrames, checkpointed int
		err = db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
		_ = db.Close()
		if err != nil {
			return fmt.Errorf("checkpoint %s: %w", oldDB, err)
		}
		if busy != 0 {
			return fmt.Errorf("数据库仍被占用，停止迁移：%s", oldDB)
		}
		if err := os.Rename(oldDB, newDB); err != nil {
			return err
		}
		_ = os.Remove(oldDB + "-wal")
		_ = os.Remove(oldDB + "-shm")
	}
	if logDir != "" {
		oldLog, newLog := filepath.Join(logDir, legacyName+".log"), filepath.Join(logDir, "rymesh.log")
		if fileExists(oldLog) && !fileExists(newLog) {
			_ = os.Rename(oldLog, newLog)
		}
	}
	return nil
}

// migrateLegacyHome moves a personal install (~/.agentconsole: a Mac client
// or a node set up over SSH) to ~/.rymesh. The old console or node service is
// stopped and unregistered first; the caller starts the new one.
func migrateLegacyHome() error {
	h, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	old, base := filepath.Join(h, "."+legacyName), filepath.Join(h, ".rymesh")
	hasLegacy := isRealDir(old) || stopLegacyServices(h, base, true)
	if !hasLegacy {
		return nil
	}
	stopLegacyServices(h, old, false)
	if _, err := moveLegacyDir(old, base); err != nil {
		return fmt.Errorf("移动 %s 失败：%w", old, err)
	}
	cfgPath := filepath.Join(base, "config.json")
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取迁移后的配置失败：%w", err)
	}
	if cfg.renameLegacyPaths([][2]string{{old, base}}) {
		if err := saveConfig(cfgPath, cfg); err != nil {
			return err
		}
	}
	return migrateLegacyFiles(cfg.DataDir, cfg.LogDir)
}

// stopLegacyServices stops a console or node registered under the old names
// (and a detached one recorded in base/node.pid) and removes the
// registrations. With probe it only reports whether any registration exists.
func stopLegacyServices(home, base string, probe bool) bool {
	found := false
	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist")
		if fileExists(plist) {
			found = true
			if !probe {
				uid := fmt.Sprint(os.Getuid())
				for _, d := range []string{"gui/" + uid, "user/" + uid} {
					_ = exec.Command("launchctl", "bootout", d+"/"+legacyLaunchdLabel).Run()
				}
				_ = os.Remove(plist)
			}
		}
	case "linux":
		user := filepath.Join(home, ".config", "systemd", "user", legacyNodeUnit+".service")
		system := "/etc/systemd/system/" + legacyNodeUnit + ".service"
		if fileExists(user) {
			found = true
			if !probe {
				_ = systemctl(true, "disable", "--now", legacyNodeUnit)
				_ = os.Remove(user)
				_ = systemctl(true, "daemon-reload")
			}
		}
		if os.Geteuid() == 0 && fileExists(system) {
			found = true
			if !probe {
				_ = systemctl(false, "disable", "--now", legacyNodeUnit)
				_ = os.Remove(system)
				_ = systemctl(false, "daemon-reload")
			}
		}
	}
	if !probe {
		stopDetached(base)
	}
	return found
}

// migrateLegacySystem moves a system install (`agentconsole install`) to the
// new directories and service name. The old service is stopped and removed;
// agent tasks keep running (KillMode=process). The old binary is kept as
// <path>.prev with a link to exe in its place. The caller (install) writes and
// starts rymesh.service.
func migrateLegacySystem(exe string) error {
	unit := "/etc/systemd/system/" + legacyUnit + ".service"
	if !fileExists(legacyConfigPath) && !fileExists(unit) {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("迁移系统级安装需要 root")
	}
	fmt.Println("迁移旧版安装（agentconsole → rymesh）")
	oldBin := ""
	if out, err := runSystemctl("show", "-p", "ExecStart", "--value", legacyUnit); err == nil {
		if i := strings.Index(out, "path="); i >= 0 {
			oldBin = strings.Fields(strings.NewReplacer(";", " ").Replace(out[i+5:]))[0]
		}
	}
	if fileExists(unit) {
		_, _ = runSystemctl("disable", "--now", legacyUnit)
		_ = os.Remove(unit)
		_, _ = runSystemctl("daemon-reload")
	}
	for _, pr := range legacySystemDirs {
		if _, err := moveLegacyDir(pr[0], pr[1]); err != nil {
			return fmt.Errorf("移动 %s 失败：%w", pr[0], err)
		}
	}
	if cfg, err := loadConfig(defaultConfigPath); err == nil {
		if cfg.renameLegacyPaths(legacySystemDirs) {
			if err := saveConfig(defaultConfigPath, cfg); err != nil {
				return err
			}
		}
		if err := migrateLegacyFiles(cfg.DataDir, cfg.LogDir); err != nil {
			return err
		}
	}
	if oldBin != "" && oldBin != exe && fileExists(oldBin) {
		prev := oldBin + ".prev"
		_ = os.Remove(prev)
		if os.Rename(oldBin, prev) == nil {
			_ = os.Symlink(exe, oldBin)
		}
	}
	// Give the stopped service's port a moment to free up.
	time.Sleep(500 * time.Millisecond)
	return nil
}
