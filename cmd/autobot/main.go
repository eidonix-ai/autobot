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
