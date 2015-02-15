// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drive

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	gopath "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/odeke-em/drive/config"
	"github.com/odeke-em/dts/trie"
)

// Pushes to remote if local path exists and in a gd context. If path is a
// directory, it recursively pushes to the remote if there are local changes.
// It doesn't check if there are local changes if isForce is set.
func (g *Commands) Push() (err error) {
	defer g.clearMountPoints()

	root := g.context.AbsPathOf("")
	var cl []*Change

	fmt.Println("Resolving...")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	// To Ensure mount points are cleared in the event of external exceptios
	go func() {
		_ = <-c
		g.clearMountPoints()
		os.Exit(1)
	}()

	for _, relToRootPath := range g.opts.Sources {
		fsPath := g.context.AbsPathOf(relToRootPath)
		ccl, cErr := g.changeListResolve(relToRootPath, fsPath, true)
		if cErr != nil {
			return cErr
		}
		if len(ccl) > 0 {
			cl = append(cl, ccl...)
		}
	}

	mount := g.opts.Mount
	if mount != nil {
		for _, mt := range mount.Points {
			ccl, cerr := lonePush(g, root, mt.Name, mt.MountPath)
			if cerr == nil {
				cl = append(cl, ccl...)
			}
		}
	}

	nonConflicts, conflicts := sift(cl)
	resolved, unresolved := resolveConflicts(conflicts, true, g.deserializeIndex)
	if len(unresolved) >= 1 {
		if conflictsPersist(unresolved) {
			return
		}
		for _, ch := range unresolved {
			resolved = append(resolved, ch)
		}
	}

	for _, ch := range resolved {
		nonConflicts = append(nonConflicts, ch)
	}

	ok := printChangeList(nonConflicts, g.opts.NoPrompt, g.opts.NoClobber)
	if ok {
		pushSize := reduceToSize(cl, true)

		quotaStatus, qErr := g.QuotaStatus(pushSize)
		if qErr != nil {
			return qErr
		}
		unSafe := false
		switch quotaStatus {
		case AlmostExceeded:
			fmt.Println("\033[92mAlmost exceeding your drive quota\033[00m")
		case Exceeded:
			fmt.Println("\033[91mThis change will exceed your drive quota\033[00m")
			unSafe = true
		}
		if unSafe {
			fmt.Printf(" projected size: %d (%d)\n", pushSize, prettyBytes(pushSize))
			if !promptForChanges() {
				return
			}
		}
		return g.playPushChangeList(nonConflicts)
	}
	return
}

func (g *Commands) deserializeIndex(identifier string) *config.Index {
	index, err := g.context.DeserializeIndex(g.context.AbsPathOf(""), identifier)
	if err != nil {
		return nil
	}
	return index
}

func (g *Commands) playPushChangeList(cl []*Change) (err error) {
	g.taskStart(len(cl))

	// TODO: Only provide precedence ordering if all the other options are allowed
	// Currently noop on sorting by precedence
	if false && !g.opts.NoClobber {
		sort.Sort(ByPrecedence(cl))
	}

	var adds, mods, dels []*Change

	for _, c := range cl {
		switch c.Op() {
		case OpMod:
			mods = append(mods, c)
		case OpModConflict:
			mods = append(mods, c)
		case OpAdd:
			adds = append(adds, c)
		case OpDelete:
			dels = append(dels, c)
		}
	}

	g.scheduleAdds(adds)
	g.scheduleMods(mods)
	g.scheduleDels(dels)

	// Time to organize them according branching
	g.taskFinish()
	return err
}

func (g *Commands) scheduleDels(cl []*Change) (err error) {
	for _, c := range cl {
		g.remoteDelete(c)
	}
	return
}

func (g *Commands) scheduleUpserts(cl []*Change, f func(*Change) error) (err error) {
	tr := trie.New(trie.AsciiAlphabet)
	for _, c := range cl {
		tr.Set(c.Path, c.Path)
	}

	dir := "dir"

	_ = tr.Tag(trie.PotentialDir, dir)
	potentialDirs := tr.Match(trie.PotentialDir)

	eos := func(tn *trie.TrieNode) bool {
		return tn != nil && tn.Eos
	}

	for match := range potentialDirs {
		endNodes := match.Match(eos)
		prefixes := []string{}
		for node := range endNodes {
			prefixes = append(prefixes, node.Data.(string))
		}
		if len(prefixes) < 1 {
			continue
		}

		prefix := commonPrefix(prefixes...)
		prefix = strings.TrimRight(prefix, UnescapedPathSep)

		_, pErr := g.mkdirAll(prefix)
		if pErr != nil {
			return pErr
		}
	}

	for _, c := range cl {
		f(c)
	}
	return
}

func (g *Commands) scheduleMods(cl []*Change) (err error) {
	return g.scheduleUpserts(cl, g.remoteMod)
}

func (g *Commands) scheduleAdds(cl []*Change) (err error) {
	return g.scheduleUpserts(cl, g.remoteAdd)
}

func lonePush(g *Commands, parent, absPath, path string) (cl []*Change, err error) {
	r, err := g.rem.FindByPath(absPath)
	if err != nil && err != ErrPathNotExists {
		return
	}

	var l *File
	localinfo, _ := os.Stat(path)
	if localinfo != nil {
		l = NewLocalFile(path, localinfo)
	}

	return g.resolveChangeListRecv(true, parent, absPath, r, l)
}

func parentPath(p string) string {
	d := strings.Split(p, "/")
	d = append([]string{"/"}, d[:len(d)-1]...)
	return gopath.Join(d...)
}

func (g *Commands) remoteMod(change *Change) (err error) {
	defer g.taskDone()

	absPath := g.context.AbsPathOf(change.Path)
	var parent *File
	if change.Dest != nil {
		change.Src.Id = change.Dest.Id // TODO: bad hack
	}

	parPath := parentPath(change.Path)
	parent, err = g.rem.FindByPath(parPath)
	if err != nil {
		return err
	}

	args := upsertOpt{
		parentId:       parent.Id,
		fsAbsPath:      absPath,
		src:            change.Src,
		dest:           change.Dest,
		mask:           g.opts.TypeMask,
		ignoreChecksum: g.opts.IgnoreChecksum,
	}

	rem, err := g.rem.UpsertByComparison(&args)
	if err != nil {
		fmt.Printf("%s: %v\n", change.Path, err)
		return
	}
	if rem == nil {
		return
	}
	index := rem.ToIndex()
	wErr := g.context.SerializeIndex(index, g.context.AbsPathOf(""))

	// TODO: Should indexing errors be reported?
	if wErr != nil {
		fmt.Printf("serializeIndex %s: %v\n", rem.Name, wErr)
	}
	return
}

func (g *Commands) remoteAdd(change *Change) (err error) {
	return g.remoteMod(change)
}

func (g *Commands) indexAbsPath(fileId string) string {
	return config.IndicesAbsPath(g.context.AbsPathOf(""), fileId)
}

func (g *Commands) remoteUntrash(change *Change) (err error) {
	defer g.taskDone()

	return g.rem.Untrash(change.Src.Id)
}

func (g *Commands) remoteDelete(change *Change) (err error) {
	defer g.taskDone()

	err = g.rem.Trash(change.Dest.Id)
	if err != nil {
		return
	}

	indexPath := g.indexAbsPath(change.Dest.Id)
	if rmErr := os.Remove(indexPath); rmErr != nil {
		fmt.Printf("%s \"%s\": remove indexfile %v\n", change.Path, change.Dest.Id, rmErr)
	}
	return
}

func (g *Commands) mkdirAll(d string) (file *File, err error) {
	// Try the lookup one last time in case a coroutine raced us to it.
	retrFile, retryErr := g.rem.FindByPath(d)
	if retryErr == nil && retrFile != nil {
		return retrFile, nil
	}

	rest, last := filepath.Split(strings.TrimRight(d, UnescapedPathSep))
	if rest == "" || last == "" {
		return nil, fmt.Errorf("cannot tamper with root")
	}

	parent, parentErr := g.rem.FindByPath(rest)
	if parentErr != nil && parentErr != ErrPathNotExists {
		return parent, parentErr
	}

	if parent == nil {
		parent, parentErr = g.mkdirAll(rest)
		if parentErr != nil || parent == nil {
			return parent, parentErr
		}
	}

	remoteFile := &File{
		IsDir: true,
		Name:  last,
	}

	args := upsertOpt{
		parentId: parent.Id,
		src:      remoteFile,
	}
	parent, parentErr = g.rem.UpsertByComparison(&args)
	if parentErr == nil && parent != nil {
		index := parent.ToIndex()
		wErr := g.context.SerializeIndex(index, g.context.AbsPathOf(""))

		// TODO: Should indexing errors be reported?
		if wErr != nil {
			fmt.Printf("serializeIndex %s: %v\n", parent.Name, wErr)
		}
	}
	return parent, parentErr
}

func list(context *config.Context, p string, hidden bool) (fileChan chan *File, err error) {
	absPath := context.AbsPathOf(p)
	var f []os.FileInfo
	f, err = ioutil.ReadDir(absPath)
	fileChan = make(chan *File)
	if err != nil {
		close(fileChan)
		return
	}

	go func() {
		for _, file := range f {
			if file.Name() == config.GDDirSuffix {
				continue
			}
			if !isHidden(file.Name(), hidden) {
				fileChan <- NewLocalFile(gopath.Join(absPath, file.Name()), file)
			}
		}
		close(fileChan)
	}()
	return
}
