// Command ripperx serves the optical drives of a machine over HTTP: what
// they are and what they can do, what is in them, the files on that disc,
// the audio on it, an exact image of it, and - when the drive can write -
// a disc burned from an image with the result read back and checked.
//
// It talks to the drives itself, in SCSI MMC, rather than through a mount
// or a helper program. Burning is the one exception: that is handed to
// xorriso or cdrecord, which have met far more drive firmware than this
// ever will. The checking before and after a burn is ours.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// version is stamped by the release build; a development build says so.
var version = "development build"

// Defaults, all in one place. Each is also a command-line flag, so these
// are the values you get with no arguments.
const (
	defaultAddr      = "127.0.0.1:8080"
	defaultOutDir    = "."
	defaultUploadMax = 8 << 30 // 8 GiB: a dual-layer DVD image and room to spare

	// A short life for the token every request carries, and a long one for
	// the token that silently renews it.
	defaultAuthTTL        = 15 * time.Minute
	defaultAuthRefreshTTL = 365 * 24 * time.Hour
)

type server struct {
	drives  *driveSet
	jobs    *jobManager
	store   store
	burner  *burner
	history *history
	// isos is the read-only library of installer images to burn from. It is
	// a separate store rather than another directory in the same one so
	// that nothing can write to it by accident.
	isos store
	// isoFacts is what has been read out of those images already: which
	// firmware and architecture each one boots, and the disc it needs.
	isoFacts *factCache

	uploadMax   int64
	readSpeedKB int
	allowBurn   bool
	allowEject  bool
	authOn      bool
	startedAt   time.Time

	// web is the embedded page directory, and api the route table both the
	// mux and the API document are built from.
	web      fs.FS
	api      []route
	specOnce sync.Once
	spec     []byte
	specErr  error
}

func main() {
	addr := flag.String("addr", defaultAddr, "address to listen on")
	out := flag.String("out", defaultOutDir,
		"directory to keep finished images, rips and uploads in")
	devices := flag.String("devices", "",
		"comma-separated device nodes to offer, such as /dev/sr0,/dev/sr1; "+
			"empty means every optical drive found on this machine")
	readSpeed := flag.Int("read-speed-kb", 0,
		"cap the drive's read speed, in kilobytes a second; 0 leaves the drive alone. "+
			"176 is 1x on a CD. Slowing a drive down is what rescues a scratched disc")
	burnerPath := flag.String("burner", "",
		"the burner program to use; empty searches for xorriso, then cdrecord, then wodim")
	allowBurn := flag.Bool("allow-burn", true,
		"offer burning at all. With this off ripperX can only read discs, which is "+
			"the right setting for a machine whose drives are shared out read-only")
	allowEject := flag.Bool("allow-eject", true,
		"let the page open and close the drive trays")
	uploadMax := flag.Int64("upload-max", defaultUploadMax,
		"largest image, in bytes, that may be uploaded to be burned")
	historyPath := flag.String("history", "",
		"the SQLite file scans and finished jobs are recorded in; empty puts it beside "+
			"the images when those are local, and in the working directory when they are on a share. "+
			"\"off\" turns the history off")
	configPath := flag.String("config", defaultConfigPath,
		"settings file; ignored if it does not exist")
	smbAddress := flag.String("smb-address", "",
		"keep images on an SMB share instead of a local directory, as //host/share[/subdir]")
	isoStore := flag.String("iso-store", "",
		"a read-only library of images to burn from, as //host/share/path or a local "+
			"directory. Nothing is ever written to it. It uses the smb-user and "+
			"smb-password settings unless iso-store-user is set")
	isoUser := flag.String("iso-store-user", "",
		"user for the iso-store share, when it is not the same as smb-user")
	isoDomain := flag.String("iso-store-domain", "",
		"domain for the iso-store share, when it is not the same as smb-domain")
	smbUser := flag.String("smb-user", "", "user to log in to the SMB share as")
	smbDomain := flag.String("smb-domain", "", "domain or workgroup for the SMB login")
	authUser := flag.String("auth-user", "",
		"user name for the login page; with auth-password in the settings file, it turns authentication on")
	authTTL := flag.Duration("auth-ttl", defaultAuthTTL,
		"how long a login token is accepted for before it is renewed from the refresh token")
	authRefreshTTL := flag.Duration("auth-refresh-ttl", defaultAuthRefreshTTL,
		"how long a browser stays logged in without typing the password again")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ripperX", version)
		return
	}

	// The file fills in whatever the command line did not, so a service can
	// be configured entirely from /etc/ripperx.conf.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	smbPassword, isoPassword, authPassword, jwtSecret := "", "", "", ""
	if cfg != nil {
		if err := cfg.apply(flag.CommandLine, explicitFlags(flag.CommandLine)); err != nil {
			log.Fatal(err)
		}
		for _, key := range secretKeys {
			if err := cfg.checkSecret(key); err != nil {
				log.Fatal(err)
			}
		}
		smbPassword = cfg.get("smb-password")
		isoPassword = cfg.get("iso-store-password")
		authPassword = cfg.get("auth-password")
		jwtSecret = cfg.get("jwt-secret")
	}

	guard, err := newAuth(*authUser, authPassword, jwtSecret, *authTTL, *authRefreshTTL)
	if err != nil {
		log.Fatal(err)
	}

	st, err := openStore(*smbAddress, *smbUser, smbPassword, *smbDomain, *out)
	if err != nil {
		log.Fatal(err)
	}

	// The ISO library shares the main credentials unless it was given its
	// own, because in practice it is another folder on the same server.
	if *isoUser == "" {
		*isoUser, isoPassword, *isoDomain = *smbUser, smbPassword, *smbDomain
	}
	isos, err := openReadOnlyStore(*isoStore, *isoUser, isoPassword, *isoDomain)
	if err != nil {
		log.Fatalf("iso-store: %v", err)
	}

	// A history that cannot be opened is reported and then done without:
	// ripperX drives hardware, and refusing to rip a disc because a log
	// file is on a read-only filesystem would be the wrong trade.
	var hist *history
	if *historyPath != "off" {
		p := *historyPath
		if p == "" {
			p = defaultHistoryPath(st, *out)
		}
		hist, err = openHistory(p)
		if err != nil {
			log.Printf("warning: %v; scans and jobs will not be remembered across a restart", err)
			hist = nil
		}
	}

	paths, err := discoverDrives(*devices)
	if err != nil {
		log.Fatal(err)
	}
	if len(paths) == 0 {
		log.Print("warning: no optical drive found. ripperX will serve, but with nothing to offer; " +
			"pass -devices if yours is not named /dev/sr*")
	}

	s := &server{
		drives:      newDriveSet(paths),
		store:       st,
		history:     hist,
		isos:        isos,
		isoFacts:    newFactCache(),
		burner:      findBurner(*burnerPath),
		uploadMax:   *uploadMax,
		readSpeedKB: *readSpeed,
		allowBurn:   *allowBurn,
		allowEject:  *allowEject,
		authOn:      guard != nil,
		startedAt:   time.Now(),
	}
	s.jobs = newJobManager(s)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	s.web = sub
	s.api = s.routes(guard)

	mux := http.NewServeMux()
	for _, rt := range s.api {
		mux.Handle(rt.Pattern, methodGuard(rt, rt.Handler))
	}
	// The pages themselves are static files rather than endpoints, so they
	// are not in the route table. /login is the one that needs a name of its
	// own: it is where the guard sends a browser with no token, and a
	// redirect to /login.html would put the extension in front of the user
	// on every sign-in.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, s.web, "login.html")
	})
	mux.Handle("/", http.FileServerFS(s.web))

	var handler http.Handler = mux
	if guard != nil {
		handler = guard.guard(mux)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: handler,
		// No write timeout: ripping a disc streams for as long as the disc
		// takes, and a burn takes longer still.
		ReadHeaderTimeout: 20 * time.Second,
	}

	log.Printf("ripperX %s", version)
	log.Printf("drives: %s", strings.Join(paths, ", "))
	log.Printf("images: %s", st.Describe())
	if isos != nil {
		log.Printf("iso library: %s (read only)", isos.Describe())
	}
	if hist != nil {
		log.Printf("history: %s", hist.path)
	} else {
		log.Print("history: off - scans and jobs are forgotten when this process stops")
	}
	if s.burner != nil {
		log.Printf("burner: %s (%s)", s.burner.path, s.burner.kind)
	} else if *allowBurn {
		log.Print("burner: none found - install xorriso to burn discs")
	}
	if guard == nil {
		log.Print("authentication: off - anyone who can reach this port can use the drives")
	} else {
		log.Printf("authentication: on, as %s", *authUser)
	}
	log.Printf("listening on http://%s", *addr)

	// A drive left with its tray locked, or a job half-written, is worth
	// tidying: shut the jobs down first so nothing is writing when the
	// process goes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		s.jobs.cancelAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	s.drives.closeAll()
	_ = hist.close()
}

// methodGuard answers 405 rather than running a handler for the wrong verb,
// so the route table's Method column is enforced and not merely documented.
func methodGuard(rt route, h http.HandlerFunc) http.Handler {
	allowed := append([]string{rt.Method}, rt.Extra...)
	if rt.Method == http.MethodGet {
		allowed = append(allowed, http.MethodHead)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range allowed {
			if r.Method == m {
				h(w, r)
				return
			}
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{
			Error: fmt.Sprintf("this endpoint takes %s, not %s",
				strings.Join(allowed, " or "), r.Method)})
	})
}

// discoverDrives returns the device nodes to offer. An explicit list is
// taken as given - including a node that does not exist yet, since a USB
// drive may be plugged in later - and an empty one is filled by looking for
// the names Linux gives optical drives.
func discoverDrives(explicit string) ([]string, error) {
	if explicit != "" {
		var out []string
		for _, p := range strings.Split(explicit, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	found, err := filepath.Glob("/dev/sr[0-9]*")
	if err != nil {
		return nil, err
	}
	// /dev/scd* is the same device under its other name on some systems; it
	// is only added when there is no sr* at all, so a drive is never listed
	// twice.
	if len(found) == 0 {
		if scd, err := filepath.Glob("/dev/scd[0-9]*"); err == nil {
			found = scd
		}
	}
	sort.Strings(found)
	return found, nil
}

// decodeJSON reads a small request body, refusing anything unreasonably
// large before it is parsed and reporting a malformed body in words a page
// can show.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		return errors.New("malformed request")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error" doc:"what went wrong, in the same words the log gets"`
}

type statusMessage struct {
	Status string `json:"status" doc:"what the server did"`
}

func writeErr(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	writeJSON(w, code, errorResponse{Error: err.Error()})
}

// statusResponse is what the page reads once, to know what this server is
// and what it will let the user do, before it draws anything.
type statusResponse struct {
	Version     string `json:"version" doc:"which build of ripperX this is"`
	Store       string `json:"store" doc:"where images are kept, with no password in it"`
	StoreKind   string `json:"storeKind" doc:"local or smb"`
	Burner      string `json:"burner" doc:"the burner program found, or empty if there is none"`
	BurnerKind  string `json:"burnerKind" doc:"xorriso, cdrecord or wodim"`
	AllowBurn   bool   `json:"allowBurn" doc:"whether this server offers burning at all"`
	AllowEject  bool   `json:"allowEject" doc:"whether the page may open and close trays"`
	AuthOn      bool   `json:"authOn" doc:"whether a login is required"`
	History     string `json:"history,omitempty" doc:"where scans and finished jobs are recorded, or empty when they are not"`
	ISOStore    string `json:"isoStore,omitempty" doc:"the read-only library of images to burn from, when one is configured"`
	UploadMax   int64  `json:"uploadMax" doc:"largest upload accepted, in bytes"`
	ReadSpeedKB int    `json:"readSpeedKb" doc:"read speed cap applied to every rip, or 0 for the drive's own"`
	Uptime      string `json:"uptime" doc:"how long this server has been running"`
	Drives      int    `json:"drives" doc:"how many drives are configured"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResponse{
		Version:     version,
		Store:       s.store.Describe(),
		StoreKind:   s.store.Kind(),
		AllowBurn:   s.allowBurn && s.burner != nil,
		AllowEject:  s.allowEject,
		AuthOn:      s.authOn,
		UploadMax:   s.uploadMax,
		ReadSpeedKB: s.readSpeedKB,
		Uptime:      time.Since(s.startedAt).Round(time.Second).String(),
		Drives:      s.drives.count(),
	}
	if s.burner != nil {
		resp.Burner, resp.BurnerKind = s.burner.path, s.burner.kind
	}
	if s.history != nil {
		resp.History = s.history.path
	}
	if s.isos != nil {
		resp.ISOStore = s.isos.Describe()
	}
	writeJSON(w, http.StatusOK, resp)
}
