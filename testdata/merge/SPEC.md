# Three-way merge scenarios

This directory holds the input generator for go-git's three-way merge work and
the contract that the merge command has to meet. Real git is the reference.
Every expected answer comes from running git 2.43.0 on the same repository, so
nothing here stores answers.

## The generator

`gen/main.go` is a standalone program (Go standard library plus the git binary)
that writes merge scenarios as git repositories:

    go run ./testdata/merge/gen -out DIR -seed 1 -count 500
    go run ./testdata/merge/gen -out DIR -case all
    go run ./testdata/merge/gen -list

It prints one line per scenario, `<name> <family> <path>`. Seeded scenarios are
named `s<seed>` and are bare repositories. Hand-built cases carry their own
names. The same seed builds the same commit ids on any machine and with any git
from 2.38 on, because the generator writes objects through `git fast-import`
with a fixed identity, fixed commit dates (one hour apart, all distinct) and its
own random number generator. It sets `GIT_CONFIG_NOSYSTEM=1` and
`GIT_CONFIG_GLOBAL=/dev/null`, so the user's git configuration never reaches
it. Set `GIT` to use a git binary other than the one on `PATH`.

Every scenario has two branches, `A` and `B`. A merge always means merging `B`
into `A`, with `A` named first.

Seeds 1 to 100000 are the development range. Other seeds are used for
comparison.

Each seed picks one family. About a third of the seeds are criss-cross
histories, a quarter are overlapping edits, a fifth are renames, and the
other four families share the rest:

- `crisscross`: two branches that merged each other, so the final merge has
  more than one merge base and needs a virtual base built by merging the bases
  first. There are three shapes: two merge bases; three, where a third branch
  was merged into both; and two rounds of cross merges, where the two merge
  bases have two merge bases of their own, so the virtual base is itself built
  from a virtual base. Each merge commit keeps its own side's version of a file
  that both sides changed. When both sides changed the same lines the merge
  bases conflict, and the virtual base then holds conflict markers; when they
  changed lines far apart the bases merge cleanly.
- `edit`: line edits on both sides. Most seeds have both branches working on
  the same few lines of one file: several changes a line or two apart, some
  made differently on the two sides, some identical on both (a helper or fix
  both branches picked up) and some on one side only. How git groups those
  changes into conflict hunks decides the merged text.
- `rename`: renames with and without an edit, to the same directory, to
  another directory under the same basename, or anywhere. Paired with an edit,
  a delete, a second rename (rename/rename in both directions) or an add at the
  target. Some renamed files are executable scripts, some renames also flip the
  executable bit, and some carry a heavy edit. Base trees sometimes hold two
  near-identical files.
- `mode`: the executable bit flipped on one or both sides, with edits and
  renames.
- `adddel`: added and deleted files, add/add on one path (same content, or two
  edits of one draft), modify/delete and delete/delete.
- `filedir`: a file replaced by a directory of the same name, or a directory
  replaced by a file, against edits, adds and deletes on the other side.
- `mixed`: any of the above together.

Outside `edit`, most seeds also have both sides working on the same lines of
one file. Files are Go-like source, prose, and shell scripts (`*.sh`, usually
executable).

All files are text with LF line endings. There are no symlinks, submodules,
binary files, `.gitattributes` files or CRLF content. Modes are `100644` and
`100755` only.

`-case all` builds the hand-built cases listed by `-list`. The three
`cherry-pick-*` cases are non-bare repositories with `A` checked out. In each,
the tip of `B` is the commit to pick.

## The oracle

The reference answer for a scenario at `<path>` is what this prints, together
with its exit status (0 clean, 1 conflicted):

    env -i PATH="$PATH" HOME="$EMPTY_DIR" LC_ALL=C \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
      git -C <path> -c merge.conflictStyle=merge -c merge.renames=true \
        -c merge.directoryRenames=false -c diff.renames=true \
        merge-tree --write-tree --name-only --no-messages A B

The first line is the id of the merged tree. Conflicted files are written into
that tree with git's conflict markers, labelled `A` and `B` as git labels them,
so the tree id also fixes the merged content of every file. The lines
after it are the paths git reports as conflicted, in git's order.

The git version is 2.43.0. Git 2.38.0, 2.43.0 and 2.53.0 gave identical answers
on seeds 1 to 3000. From 2.54 on, git re-diffs shifted change groups in its
histogram diff, which changed 5 of those 3000 seeds; match 2.43.0 where they
disagree. The repository configuration written by the generator holds the
same settings as the `-c` flags above, plus `core.autocrlf=false`.

Out of scope: directory rename detection (off through
`merge.directoryRenames=false`), merge drivers and `.gitattributes`,
renormalization, the `diff3` and `zdiff3` conflict styles, and the
informational messages that `--messages` would print.

## The command

`cmd/mergetree` in this module is a program built on go-git's own merge code.

    mergetree <repo> <branch1> <branch2>

It opens the repository at `<repo>` (bare or not), merges `<branch2>` into
`<branch1>` and prints exactly what the oracle prints for the same arguments,
with the same exit status. It writes every object of the merged tree into the
repository, so `git ls-tree -r <tree>` and `git cat-file` work on the result
right after it runs. It reads nothing from the environment and must work
with only `HOME` (an empty directory) and `LC_ALL=C` set and no `PATH`.

    mergetree cherry-pick <worktree> <commit> theirs|ours

It calls go-git's `Worktree.CherryPick` on the checked-out branch of the
repository at `<worktree>`, with the committer `Merge Fixture
<fixture@example.invalid>` at Unix time 1700000000. The result must be what
`git cherry-pick -X theirs <commit>` (or `-X ours`) gives: a three-way merge
whose base is the picked commit's parent, with conflicting hunks resolved
toward the picked commit (`theirs`) or toward the current branch (`ours`).
Changes on the current branch that the picked commit does not touch survive.
On success the new commit is on top of the old HEAD, and the index and the
working tree match it. When a conflict remains that `-X` cannot resolve, such
as modify/delete, it exits 1 and HEAD does not move.

Neither form may start another process.
