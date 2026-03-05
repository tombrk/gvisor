// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"encoding/json"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fscheckpoint"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// FSSaveOpts holds options to Kernel.FSSave.
type FSSaveOpts struct {
	// These correspond to files specified by the fscheckpoint package, and are
	// all required. Ownership is transferred to Kernel.FSSave, even if it
	// returns a non-nil error.
	ManifestFile      io.WriteCloser
	MultiTarFile      io.WriteCloser
	PagesMetadataFile io.WriteCloser
	PagesFile         stateio.AsyncWriter

	// RunscVersion is the runsc binary version.
	RunscVersion string

	// If ExitAfterSaving is true, all processes exit with status 0 before
	// FSSave returns, whether or not it returns a non-nil error.
	ExitAfterSaving bool
}

// FSSave collects a filesystem checkpoint as specified by the fscheckpoint
// package.
func (k *Kernel) FSSave(ctx context.Context, opts *FSSaveOpts) error {
	defer func() {
		if opts.ManifestFile != nil {
			opts.ManifestFile.Close()
			opts.ManifestFile = nil
		}
		if opts.MultiTarFile != nil {
			opts.MultiTarFile.Close()
			opts.MultiTarFile = nil
		}
		if opts.PagesMetadataFile != nil {
			opts.PagesMetadataFile.Close()
			opts.PagesMetadataFile = nil
		}
		if opts.PagesFile != nil {
			opts.PagesFile.Close()
			opts.PagesFile = nil
		}
	}()

	k.Pause()
	defer k.Unpause()
	if opts.ExitAfterSaving {
		defer k.Kill(linux.WaitStatusExit(0)) // consistent with sentry/state.SaveOpts.Save
	}
	return k.quiescePausedAnd(ctx, func() error {
		var (
			asyncPageSaveWg  sync.WaitGroup
			asyncPageSaveErr error
		)
		asyncPageSaveWg.Add(1)
		apfs, err := pgalloc.StartAsyncPagesFileSave(opts.PagesFile /* transfers ownership */, func(err error) {
			defer asyncPageSaveWg.Done()
			asyncPageSaveErr = err
		})
		opts.PagesFile = nil
		if err != nil {
			return fmt.Errorf("failed to start async page saving: %w", err)
		}
		asyncPageSaveCleanup := cleanup.Make(func() {
			apfs.MemoryFilesDone()
			asyncPageSaveWg.Wait()
		})
		defer asyncPageSaveCleanup.Clean()
		mfOpts := pgalloc.SaveOpts{
			PagesFile: apfs,
		}

		manifest := fscheckpoint.Manifest{
			RunscVersion: opts.RunscVersion,
		}
		type tmpfsAndPrivateMemoryFile struct {
			fs *vfs.Filesystem
			mf *pgalloc.MemoryFile
		}
		restoreIDs := make(map[string]tmpfsAndPrivateMemoryFile)
		multiTarWriter := &countingWriter{w: opts.MultiTarFile}
		pagesMetadataWriter := &countingWriter{w: opts.PagesMetadataFile}
		prevTarOffset := uint64(0)
		prevPagesMetadataOffset := uint64(0)
		prevPagesOffset := uint64(0)
		fss := k.vfs.GetFilesystems()
		defer func() {
			for _, fs := range fss {
				fs.DecRef(ctx)
			}
		}()
		// TODO: fss is obtained by iterating a map, so its order - and thus
		// the order in which filesystems will be saved - is effectively
		// random. pgalloc.MemoryFile async page loading biases toward
		// MemoryFiles stored at lower offsets in the pages file. Consider if
		// we should write filesystems in any particular order.
		for _, fs := range fss {
			if mf := tmpfs.MemoryFileOf(fs); mf != nil {
				if restoreID := mf.RestoreID(); restoreID != "" {
					if old, ok := restoreIDs[restoreID]; ok {
						return fmt.Errorf("pgalloc.MemoryFile restore ID %q is used by both tmpfs filesystem %p (MemoryFile %p) and tmpfs filesystem %p (MemoryFile %p)", restoreID, old.fs, old.mf, fs, mf)
					}
					restoreIDs[restoreID] = tmpfsAndPrivateMemoryFile{fs, mf}
					if log.IsLogging(log.Debug) {
						log.Debugf("Filesystem checkpoint saving tmpfs with restoreID %s", restoreID)
					}
					if err := mf.SaveTo(ctx, pagesMetadataWriter, &mfOpts); err != nil {
						return fmt.Errorf("failed to save MemoryFile with restoreID %s: %w", restoreID, err)
					}
					pagesEnd := apfs.PagesFileOffset()
					manifest.MemoryFiles = append(manifest.MemoryFiles, fscheckpoint.MemoryFile{
						RestoreID:          restoreID,
						PagesMetadataStart: prevPagesMetadataOffset,
						PagesMetadataEnd:   pagesMetadataWriter.count,
						PagesStart:         prevPagesOffset,
						PagesEnd:           pagesEnd,
					})
					prevPagesMetadataOffset = pagesMetadataWriter.count
					prevPagesOffset = pagesEnd
					if err := tmpfs.FSCheckpointWrite(ctx, fs, multiTarWriter); err != nil {
						return fmt.Errorf("failed to write tmpfs with restoreID %s to multi-tar file: %w", restoreID, err)
					}
					manifest.Tmpfs = append(manifest.Tmpfs, fscheckpoint.Tmpfs{
						MemoryFileRestoreID: restoreID,
						TarStart:            prevTarOffset,
						TarEnd:              multiTarWriter.count,
					})
					prevTarOffset = multiTarWriter.count
				}
			}
		}
		if len(restoreIDs) == 0 {
			return fmt.Errorf("no checkpointable filesystems")
		}
		apfs.MemoryFilesDone()
		if err := json.NewEncoder(opts.ManifestFile).Encode(manifest); err != nil {
			return fmt.Errorf("failed to write manifest file: %w", err)
		}
		// Close other writers while MemoryFile saving is in progress to
		// overlap their latencies.
		err = opts.ManifestFile.Close()
		opts.ManifestFile = nil
		if err != nil {
			return fmt.Errorf("failed to close manifest file: %w", err)
		}
		err = opts.MultiTarFile.Close()
		opts.MultiTarFile = nil
		if err != nil {
			return fmt.Errorf("failed to close multi-tar file: %w", err)
		}
		err = opts.PagesMetadataFile.Close()
		opts.PagesMetadataFile = nil
		if err != nil {
			return fmt.Errorf("failed to close pages metadata file: %w", err)
		}
		// Finally wait for MemoryFile saving to complete.
		asyncPageSaveCleanup.Release()()
		if asyncPageSaveErr != nil {
			return fmt.Errorf("failed to save MemoryFile pages: %w", asyncPageSaveErr)
		}
		return nil
	})
}

type countingWriter struct {
	w     io.Writer
	count uint64
}

// Write implements io.Writer.Write.
func (cw *countingWriter) Write(src []byte) (int, error) {
	n, err := cw.w.Write(src)
	cw.count += uint64(n)
	return n, err
}
