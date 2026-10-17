package config

import (
	"fmt"
	"net"
	"path"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Archive target types.
const (
	TypeSMB    = "smb"
	TypeNFS    = "nfs"
	TypeRsync  = "rsync"
	TypeRclone = "rclone"
)

// ArchiveConfig controls copying clips to a NAS, or any other archive
// target, when the car is home.
type ArchiveConfig struct {
	Enabled bool `toml:"enabled"`
	// Type is the kind of target: smb, nfs, rsync or rclone. Its settings
	// are in the table of the same name, e.g. [archive.smb].
	Type string `toml:"type"`
	// CheckAddress is a host:port that only answers at home; while it does,
	// the car is home. Empty means the target's own server (SMB, NFS and
	// rsync). Required for rclone, whose remote may be anywhere.
	CheckAddress string `toml:"check_address"`

	Sentry bool `toml:"sentry"`
	Saved  bool `toml:"saved"`
	Recent bool `toml:"recent"`

	// HomeDelay is how long the target must be reachable before archiving.
	HomeDelay Duration `toml:"home_delay"`
	// CheckInterval is how often to check whether the target is reachable.
	CheckInterval Duration `toml:"check_interval"`
	// RescanInterval is how often to look for new clips while home.
	RescanInterval Duration `toml:"rescan_interval"`

	SMB    SMBConfig    `toml:"smb"`
	NFS    NFSConfig    `toml:"nfs"`
	Rsync  RsyncConfig  `toml:"rsync"`
	Rclone RcloneConfig `toml:"rclone"`

	// The SMB settings directly in [archive], as in version 0.2.0. They're
	// still read, into SMB, but never written.
	LegacyServer      string `toml:"server,omitempty"`
	LegacyShare       string `toml:"share,omitempty"`
	LegacyFolder      string `toml:"folder,omitempty"`
	LegacyCredentials string `toml:"credentials,omitempty"`
	LegacyUsername    string `toml:"username,omitempty"`
	LegacyPassword    string `toml:"password,omitempty"`
	LegacySMBVersion  string `toml:"smb_version,omitempty"`
	LegacyParallel    int    `toml:"parallel,omitzero"`
}

// SMBConfig is an SMB share, reached with a built-in client.
type SMBConfig struct {
	Server string `toml:"server"` // e.g. "192.168.1.10"
	Share  string `toml:"share"`
	Folder string `toml:"folder"` // optional folder inside the share
	// Credentials is a root-only file with username= and password= lines.
	Credentials string `toml:"credentials"`
	// Username and Password may be given in the boot partition's
	// glovebox.toml; at first boot they're moved into Credentials.
	Username string `toml:"username"`
	Password string `toml:"password"`
	// Version pins the SMB version: "2.1" (fastest on a Pi Zero 2 W),
	// "3.0", "3.0.2", "3.1.1", or "" to negotiate the highest.
	Version string `toml:"version"`
	// Parallel is how many writes are in flight per file.
	Parallel int `toml:"parallel"`
}

// NFSConfig is an NFS export, mounted only while it's used.
type NFSConfig struct {
	Server string `toml:"server"`
	Export string `toml:"export"` // e.g. "/mnt/user/TeslaCam"
	Folder string `toml:"folder"` // optional folder inside the export
	// Options are the mount options.
	Options string `toml:"options"`
}

// RsyncConfig is a folder on a server reached with rsync over SSH, logging
// in with Glovebox's own SSH key.
type RsyncConfig struct {
	Server string `toml:"server"`
	Port   int    `toml:"port"`
	User   string `toml:"user"`
	Path   string `toml:"path"` // folder on the server, e.g. "/volume1/TeslaCam"
	// Key is Glovebox's private key. The setup page creates it and shows the
	// public key to add to the server's authorized_keys.
	Key        string `toml:"key"`
	KnownHosts string `toml:"known_hosts"`
}

// RcloneConfig is any rclone remote: cloud storage, SFTP, WebDAV, S3...
type RcloneConfig struct {
	// Remote is where clips go, e.g. "gdrive:TeslaCam".
	Remote string `toml:"remote"`
	// Config is the rclone.conf that defines the remote.
	Config string `toml:"config"`
	// Flags are extra rclone flags, e.g. "--bwlimit 2M".
	Flags string `toml:"flags"`
}

func defaultArchive() ArchiveConfig {
	return ArchiveConfig{
		Type:           TypeSMB,
		Sentry:         true,
		Saved:          true,
		HomeDelay:      Duration(20 * time.Second),
		CheckInterval:  Duration(30 * time.Second),
		RescanInterval: Duration(10 * time.Minute),
		SMB: SMBConfig{
			Share:       "TeslaCam",
			Credentials: "/etc/glovebox/smb-credentials",
			Version:     "2.1",
			Parallel:    4,
		},
		NFS: NFSConfig{
			// soft: a server that went away makes calls fail instead of hang
			Options: "vers=3,proto=tcp,nolock,soft,timeo=100,retrans=2",
		},
		Rsync: RsyncConfig{
			Port:       22,
			Key:        "/etc/glovebox/ssh/id_ed25519",
			KnownHosts: "/etc/glovebox/ssh/known_hosts",
		},
		Rclone: RcloneConfig{Config: "/etc/glovebox/rclone.conf"},
	}
}

// Server returns the host the target is on, if it has one.
func (a ArchiveConfig) Server() string {
	switch a.Type {
	case TypeSMB:
		return a.SMB.Server
	case TypeNFS:
		return a.NFS.Server
	case TypeRsync:
		return a.Rsync.Server
	}
	return ""
}

// migrateLegacy moves SMB settings given directly in [archive] (version
// 0.2.0's layout) into [archive.smb].
func (a *ArchiveConfig) migrateLegacy(md toml.MetaData) {
	set := func(key string) bool { return md.IsDefined("archive", key) }
	if set("server") {
		a.SMB.Server = a.LegacyServer
	}
	if set("share") {
		a.SMB.Share = a.LegacyShare
	}
	if set("folder") {
		a.SMB.Folder = a.LegacyFolder
	}
	if set("credentials") {
		a.SMB.Credentials = a.LegacyCredentials
	}
	if set("username") {
		a.SMB.Username = a.LegacyUsername
	}
	if set("password") {
		a.SMB.Password = a.LegacyPassword
	}
	if set("smb_version") {
		a.SMB.Version = a.LegacySMBVersion
	}
	if set("parallel") {
		a.SMB.Parallel = a.LegacyParallel
	}
	a.LegacyServer, a.LegacyShare, a.LegacyFolder, a.LegacyCredentials = "", "", "", ""
	a.LegacyUsername, a.LegacyPassword, a.LegacySMBVersion, a.LegacyParallel = "", "", "", 0
}

// Validate checks the [archive] section. Only the selected type's settings
// are checked, and only when archiving is on, except for the credentials
// that the boot partition's file may carry.
func (a ArchiveConfig) Validate() error {
	if (a.SMB.Username == "") != (a.SMB.Password == "") {
		return fmt.Errorf("archive.smb.username and archive.smb.password must be set together")
	}
	if !a.Enabled {
		return nil
	}
	if a.CheckAddress != "" {
		if _, _, err := net.SplitHostPort(a.CheckAddress); err != nil {
			return fmt.Errorf("archive.check_address %q: use host:port, e.g. 192.168.1.1:80", a.CheckAddress)
		}
	}
	if time.Duration(a.CheckInterval) < time.Second || time.Duration(a.RescanInterval) < time.Minute {
		return fmt.Errorf("archive.check_interval must be at least 1s and archive.rescan_interval at least 1m")
	}
	switch a.Type {
	case TypeSMB:
		s := a.SMB
		if s.Server == "" || s.Share == "" {
			return fmt.Errorf("archive.smb.server and archive.smb.share must be set")
		}
		if s.Parallel < 1 || s.Parallel > 16 {
			return fmt.Errorf("archive.smb.parallel must be between 1 and 16")
		}
		switch s.Version {
		case "", "2.0.2", "2.1", "3.0", "3.0.2", "3.1.1":
		default:
			return fmt.Errorf("archive.smb.version %q: use 2.1, 3.0, 3.0.2, 3.1.1 or \"\" to negotiate", s.Version)
		}
		if !path.IsAbs(s.Credentials) {
			return fmt.Errorf("archive.smb.credentials %q must be an absolute path", s.Credentials)
		}
	case TypeNFS:
		n := a.NFS
		if n.Server == "" || !path.IsAbs(n.Export) {
			return fmt.Errorf("archive.nfs.server and archive.nfs.export (an absolute path like /mnt/user/TeslaCam) must be set")
		}
		if strings.ContainsAny(n.Options, " \t") {
			return fmt.Errorf("archive.nfs.options must be comma-separated, without spaces")
		}
	case TypeRsync:
		r := a.Rsync
		if r.Server == "" || r.User == "" || r.Path == "" {
			return fmt.Errorf("archive.rsync.server, archive.rsync.user and archive.rsync.path must be set")
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("archive.rsync.port must be between 1 and 65535")
		}
		if !path.IsAbs(r.Key) || !path.IsAbs(r.KnownHosts) {
			return fmt.Errorf("archive.rsync.key and archive.rsync.known_hosts must be absolute paths")
		}
	case TypeRclone:
		r := a.Rclone
		if !strings.Contains(r.Remote, ":") {
			return fmt.Errorf("archive.rclone.remote %q: use remote:path, e.g. gdrive:TeslaCam", r.Remote)
		}
		if !path.IsAbs(r.Config) {
			return fmt.Errorf("archive.rclone.config %q must be an absolute path", r.Config)
		}
		if a.CheckAddress == "" {
			return fmt.Errorf("archive.check_address must be set for rclone: a host:port only reachable at home, e.g. your router's 192.168.1.1:80")
		}
	default:
		return fmt.Errorf("archive.type %q: use smb, nfs, rsync or rclone", a.Type)
	}
	return nil
}

// Destination identifies where clips go, so a change of target can be
// told apart from a change of other settings.
func (a ArchiveConfig) Destination() string {
	switch a.Type {
	case TypeSMB:
		return fmt.Sprintf("smb://%s/%s/%s", a.SMB.Server, a.SMB.Share, a.SMB.Folder)
	case TypeNFS:
		return fmt.Sprintf("nfs://%s%s/%s", a.NFS.Server, a.NFS.Export, a.NFS.Folder)
	case TypeRsync:
		return fmt.Sprintf("rsync://%s@%s:%d%s", a.Rsync.User, a.Rsync.Server, a.Rsync.Port, a.Rsync.Path)
	case TypeRclone:
		return "rclone:" + a.Rclone.Remote
	}
	return a.Type
}
