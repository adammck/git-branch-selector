package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-tty"

	"github.com/dustin/go-humanize"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/term"
)

type List struct {
	branches []Branch
	selected int
}

type Branch struct {
	name    string
	date    time.Time
	subject string
	isHead  bool
}

func (b *Branch) when() string {
	return humanize.Time(b.date)
}

// previous selects the branch prior to the one currently selected, or does nothing if the first branch is selected.
func (l *List) previous() {
	if l.selected > 0 {
		l.selected--
	}
}

// next selects the branch after the one currently selected, or does nothing if the last branch is selected.
func (l *List) next() {
	if l.selected < len(l.branches)-1 {
		l.selected++
	}
}

func (l *List) selectedName() string {
	return l.branches[l.selected].name
}

func getBranches(repo *git.Repository, count int) (*List, error) {
	branches := []Branch{}

	iter, err := repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("repo.Branches: %w", err)
	}

	headRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("repo.Head: %w", err)
	}

	err = iter.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()

		commit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return err
		}

		subject, _, _ := strings.Cut(commit.Message, "\n")
		isHead := ref.Hash() == headRef.Hash()

		branches = append(branches, Branch{
			name:    branchName,
			date:    commit.Committer.When,
			subject: subject,
			isHead:  isHead,
		})

		if len(branches) >= count {
			return storer.ErrStop
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("iter.ForEach: %w", err)
	}

	return &List{
		branches: branches,
		selected: 0,
	}, nil
}

func termWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80 // default
	}
	return width
}

func printBranches(list *List) uint8 {
	// build the contents of the table, unaligned
	var rows [][4]string
	for i, branch := range list.branches {
		indicator := " "
		if i == list.selected {
			indicator = "*"
		}

		rows = append(rows, [4]string{indicator, branch.name, branch.when(), branch.subject})
	}

	// find the maximum width for each column
	var cw [4]int
	for _, row := range rows {
		for c, col := range row {
			if len(col) > cw[c] {
				cw[c] = len(col)
			}
		}
	}

	tw := termWidth()

	// print the table with aligned columns
	for _, row := range rows {

		line := fmt.Sprintf("%-*s  %-*s  |  %-*s  |  %-*s",
			cw[0], row[0], // indicator
			cw[1], row[1], // branch name
			cw[2], row[2], // when
			cw[3], row[3]) // msg

		if len(line) > tw {
			line = line[:tw]
		}

		// include carriage return, to move to column zero before moving down a
		// row. this is necessary in raw mode.
		fmt.Printf(line + "\r\n")
	}

	return uint8(len(rows))
}

func main() {
	count := flag.Int("n", 10, "number of branches")
	flag.Parse()

	branch := prompt(*count)
	if branch == "" {
		return
	}

	path, err := exec.LookPath("git")
	if err != nil {
		log.Fatalf("exec.LookPath: %s", err)
	}

	fmt.Println()

	args := []string{"git", "checkout", branch}
	fmt.Printf("+ %v\n", strings.Join(args, " "))

	err = syscall.Exec(path, args, os.Environ())
	if err != nil {
		log.Fatalf("syscall.Exec: %s", err)
	}
}

func prompt(count int) string {
	repo, err := git.PlainOpen(".")
	if err != nil {
		log.Fatalf("git.PlainOpen: %s", err)
	}

	branches, err := getBranches(repo, count)
	if err != nil {
		log.Fatalf("getBranches: %s", err)
	}

	t, err := tty.Open()
	if err != nil {
		log.Fatalf("tty.Open: %s", err)
	}
	defer func() {
		err := t.Close()
		if err != nil {
			log.Fatalf("t.Close: %s", err)
		}
	}()

	// put terminal into raw mode, so we can listen for keys.
	revert, err := t.Raw()
	if err != nil {
		log.Fatalf("t.Raw: %s", err)
	}
	defer func() {
		err := revert()
		if err != nil {
			log.Fatalf("revert: %s", err)
		}
	}()

	var n uint8

	for {
		// move up to the start of the previous output, to overwrite it.
		if n > 0 {
			_, err = t.Output().WriteString(fmt.Sprintf("\x1b[%dA", n))
			if err != nil {
				log.Fatalf("t.Output().WriteString: %s", err)
			}
		}

		n = printBranches(branches)

		// read one keypress
		// damn this is complicated
		// see: https://www.asciitable.com
		buf := []rune{}
		for {
			r, err := t.ReadRune()
			if err != nil {
				log.Fatalf("t.ReadRune: %s", err)
			}
			if r == 0 {
				continue
			}
			buf = append(buf, r)
			if !t.Buffered() {
				break
			}
		}

		// to exit, press: ETX, ESC, Q, or q
		// ETX (end of text) is received when ctrl+c is pressed.
		if len(buf) == 1 && (buf[0] == 3 || buf[0] == 27 || buf[0] == 'Q' || buf[0] == 'q') {
			return ""
		}

		// press Enter (CR) to switch to selected branch and exit
		if len(buf) == 1 && buf[0] == 13 {
			return branches.selectedName()
		}

		// press up/down to change selected branch
		if len(buf) == 3 && (buf[0] == 27 && buf[1] == '[') {
			if buf[2] == 'A' { // up
				branches.previous()
			} else if buf[2] == 'B' { // down
				branches.next()
			}
		}
	}
}
