// Command autobot runs the Autobot web service and manages its users from the command line.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/web"
	"golang.org/x/term"
)

const usage = `Usage:
  autobot serve   [-db FILE] [-addr HOST:PORT]     run the web service
  autobot useradd [-db FILE] [-admin] [-name "Display Name"] USERNAME
  autobot passwd  [-db FILE] USERNAME              set a user's password (signs them out)
  autobot machine [-db FILE] add OWNER/REPO NAME   add a donated machine; prints its token once
  autobot run     [-db FILE] [-ref REF] [-gpus N] [-minutes M] OWNER/REPO MACHINE < SCRIPT
                                                   queue a test run, as the first admin; prints its id
  autobot case    [-db FILE] open OWNER/REPO NUMBER   open the case for an issue or PR (public repos)
  autobot case    [-db FILE] note OWNER/REPO NUMBER AUTHOR < TEXT
                                                   add a finalized contribution to a case
  autobot charter [-db FILE] get OWNER/REPO        print a workspace's current charter
  autobot charter [-db FILE] [-as USERNAME] set OWNER/REPO < FILE
                                                   save FILE as the next charter revision

The first visitor to the web interface creates the first admin account. useradd and passwd
are for recovery; they read the password from the terminal, or from the first line of stdin.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "useradd":
		err = useradd(os.Args[2:])
	case "passwd":
		err = passwd(os.Args[2:])
	case "charter":
		err = charter(os.Args[2:])
	case "machine":
		err = machine(os.Args[2:])
	case "run":
		err = queueRun(os.Args[2:])
	case "case":
		err = caseCmd(os.Args[2:])
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal("autobot: ", err)
	}
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "autobot.db", "SQLite database file")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := dbFlag(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	keyFile := fs.String("key", "", "file holding the hex key that encrypts GitHub tokens (default: the systemd credential secret-key, else DB.key, created if missing)")
	fs.Parse(args)

	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := loadKey(*keyFile, *db)
	if err != nil {
		return err
	}
	if err := st.UseKey(key); err != nil {
		return err
	}
	if users, err := st.Users(); err != nil {
		return err
	} else if len(users) == 0 {
		log.Printf("no users yet: the first visitor to the web interface creates the admin account")
	}
	ui, err := web.New(st, &github.Client{})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ui.Watch(ctx)
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- srv.Shutdown(shutdown)
	}()
	log.Printf("autobot listening on http://%s (database %s)", *addr, *db)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}

// loadKey reads the 32-byte key that encrypts GitHub tokens, written as 64 hex digits. Without
// -key it uses the systemd credential "secret-key" when the service has one, and otherwise
// DB.key next to the database, creating that file on first use (for development).
func loadKey(path, db string) ([]byte, error) {
	if path == "" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			path = filepath.Join(dir, "secret-key")
		}
	}
	if path == "" {
		path = db + ".key"
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			key := make([]byte, 32)
			rand.Read(key)
			if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
				return nil, err
			}
			log.Printf("created the encryption key %s: back it up separately from the database; without it saved GitHub tokens cannot be read", path)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("encryption key %s must be 64 hex digits", path)
	}
	return key, nil
}

func useradd(args []string) error {
	fs := flag.NewFlagSet("useradd", flag.ExitOnError)
	db := dbFlag(fs)
	admin := fs.Bool("admin", false, "make the user an admin")
	name := fs.String("name", "", "display name")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("useradd needs exactly one USERNAME")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	pw, err := readPassword()
	if err != nil {
		return err
	}
	u, err := st.CreateUser(fs.Arg(0), *name, pw, *admin)
	if err != nil {
		return err
	}
	role := "regular user"
	if u.IsAdmin {
		role = "admin"
	}
	fmt.Printf("added %s (%s)\n", u.Username, role)
	return nil
}

func passwd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	db := dbFlag(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("passwd needs exactly one USERNAME")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.UserByUsername(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("user %q: %w", fs.Arg(0), err)
	}
	pw, err := readPassword()
	if err != nil {
		return err
	}
	if err := st.SetPassword(u.ID, pw, ""); err != nil {
		return err
	}
	fmt.Printf("password set for %s\n", u.Username)
	return nil
}

// machine adds a donated machine to a workspace, recorded as the first admin, and prints the
// token its connector signs in with. Only the token's hash is kept, so this is the only copy.
func machine(args []string) error {
	fs := flag.NewFlagSet("machine", flag.ExitOnError)
	db := dbFlag(fs)
	fs.Parse(args)
	if fs.NArg() != 3 || fs.Arg(0) != "add" {
		return errors.New("usage: autobot machine [-db FILE] add OWNER/REPO NAME")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ws, err := st.Workspace(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("workspace %s: %w", fs.Arg(1), err)
	}
	admin, err := firstAdmin(st)
	if err != nil {
		return err
	}
	_, token, err := st.AddMachine(ws.ID, fs.Arg(2), admin)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// queueRun queues a test run on a machine, as the Run button does.
func queueRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	db := dbFlag(fs)
	ref := fs.String("ref", "", "branch, tag or commit of the repository to check out")
	gpus := fs.Int("gpus", 1, "GPUs the run needs")
	minutes := fs.Int("minutes", 30, "time limit")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: autobot run [-db FILE] [-ref REF] [-gpus N] [-minutes M] OWNER/REPO MACHINE < SCRIPT")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ws, err := st.Workspace(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("workspace %s: %w", fs.Arg(0), err)
	}
	machines, err := st.Machines(ws.ID)
	if err != nil {
		return err
	}
	var m *store.Machine
	for i := range machines {
		if machines[i].Name == fs.Arg(1) {
			m = &machines[i]
		}
	}
	if m == nil {
		return fmt.Errorf("no machine %s in %s", fs.Arg(1), ws.Name)
	}
	admin, err := firstAdmin(st)
	if err != nil {
		return err
	}
	script, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil {
		return err
	}
	commit := ""
	if *ref != "" {
		if commit, err = (&github.Client{}).CommitSHA(context.Background(), "", ws.Name, *ref); err != nil {
			return err
		}
	}
	// Without the service's key the command has no token, so it handles public repositories only.
	run, err := st.QueueRun(m, strings.TrimSpace(string(script)), *ref, commit, false, *gpus, time.Duration(*minutes)*time.Minute, admin)
	if err != nil {
		return err
	}
	fmt.Println(run.ID)
	return nil
}

// caseCmd opens a case or adds a note to one, as intake and Autobot's agents will.
func caseCmd(args []string) error {
	fs := flag.NewFlagSet("case", flag.ExitOnError)
	db := dbFlag(fs)
	fs.Parse(args)
	op := fs.Arg(0)
	if !(op == "open" && fs.NArg() == 3) && !(op == "note" && fs.NArg() == 4) {
		return errors.New("usage: autobot case [-db FILE] open OWNER/REPO NUMBER | note OWNER/REPO NUMBER AUTHOR < TEXT")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ws, err := st.Workspace(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("workspace %s: %w", fs.Arg(1), err)
	}
	number, err := strconv.Atoi(fs.Arg(2))
	if err != nil {
		return fmt.Errorf("%q is not an issue or PR number", fs.Arg(2))
	}
	if op == "open" {
		// Without the service's key the command has no token, so it reads public repositories only.
		c, created, err := web.OpenCase(context.Background(), st, &github.Client{}, "", ws, number)
		if err != nil {
			return err
		}
		what := "opened"
		if !created {
			what = "already open:"
		}
		fmt.Printf("%s %s %s, case %s\n", what, ws.Name, c.Label(), c.UUID)
		return nil
	}
	c, err := st.CaseByNumber(ws.ID, number)
	if err != nil {
		return fmt.Errorf("no case for #%d in %s: %w", number, ws.Name, err)
	}
	text, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	id, err := st.StartContribution(c.UUID, fs.Arg(3))
	if err != nil {
		return err
	}
	if err := st.SetDraft(id, string(text)); err != nil {
		return err
	}
	contrib, err := st.FinalizeContribution(id)
	if err != nil {
		return err
	}
	fmt.Printf("added lines %d-%d to %s\n", contrib.StartLine, contrib.EndLine, c.Label())
	return nil
}

func firstAdmin(st *store.Store) (*store.User, error) {
	users, err := st.Users()
	if err != nil {
		return nil, err
	}
	var first *store.User
	for i := range users {
		if users[i].IsAdmin && (first == nil || users[i].ID < first.ID) {
			first = &users[i]
		}
	}
	if first == nil {
		return nil, errors.New("there is no admin yet")
	}
	return first, nil
}

// charter prints or replaces a workspace's charter. A replacement is saved as the next revision,
// exactly as the web editor saves one, recorded under -as (default: the first admin).
func charter(args []string) error {
	fs := flag.NewFlagSet("charter", flag.ExitOnError)
	db := dbFlag(fs)
	as := fs.String("as", "", "username recorded as the author (default: the first admin)")
	fs.Parse(args)
	if fs.NArg() != 2 || (fs.Arg(0) != "get" && fs.Arg(0) != "set") {
		return errors.New("usage: autobot charter [-db FILE] [-as USERNAME] get|set OWNER/REPO")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ws, err := st.Workspace(fs.Arg(1))
	if errors.Is(err, store.ErrNotFound) {
		all, _ := st.Workspaces(&store.User{IsAdmin: true})
		var names []string
		for _, w := range all {
			names = append(names, w.Name)
		}
		return fmt.Errorf("no workspace %s; there are: %s", fs.Arg(1), strings.Join(names, ", "))
	}
	if err != nil {
		return err
	}
	cur, err := st.Charter(ws.ID, 0)
	if err != nil {
		return err
	}
	if fs.Arg(0) == "get" {
		fmt.Print(cur.Body)
		fmt.Fprintf(os.Stderr, "(revision %d, saved by %s)\n", cur.Rev, cur.Author)
		return nil
	}

	var author *store.User
	if *as != "" {
		if author, err = st.UserByUsername(*as); err != nil {
			return fmt.Errorf("user %q: %w", *as, err)
		}
	} else if author, err = firstAdmin(st); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, store.MaxCharterBytes+1))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) == "" {
		return errors.New("the new charter on stdin is empty")
	}
	rev, err := st.SaveCharter(ws.ID, cur.Rev, string(body), author)
	if err != nil {
		return err
	}
	if rev == cur.Rev {
		fmt.Printf("%s charter unchanged (revision %d)\n", ws.Name, rev)
	} else {
		fmt.Printf("saved revision %d of the %s charter, recorded as %s\n", rev, ws.Name, author.Username)
	}
	return nil
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return "", errors.New("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Password again: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("the passwords did not match")
	}
	return string(a), nil
}
