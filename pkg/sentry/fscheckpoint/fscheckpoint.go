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

// Package fscheckpoint defines the format of a gVisor filesystem checkpoint.
//
// Filesystem checkpoints can only be taken in sandboxes with runsc flag
// `-overlay2` enabled, which causes sandboxed filesystems to be overlaid with
// a mutable "upper layer" that captures filesystem changes. Each filesystem
// checkpoint contains the state of all upper layers in the sandbox.
//
// TODO: The tar file format involves a lot of padding to 512-byte boundaries.
// While e.g. gzip compression of the multi-tar file may or may not be worth
// the CPU and memory overhead, consider something simpler like run-length
// encoding of zero bytes.
package fscheckpoint

import (
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
)

// Filenames of files in a filesystem checkpoint image.
const (
	ManifestFileName      = "fscheckpoint.json"
	MultiTarFileName      = "multitar.img"
	PagesMetadataFileName = checkpointfiles.PagesMetadataFileName
	PagesFileName         = checkpointfiles.PagesFileName
)

// Manifest is the type of the JSON object stored in the manifest file.
type Manifest struct {
	// Version is the checkpoint format version.
	//
	// While Version is 0, filesystem checkpoint compatibility is not
	// guaranteed between differing runsc binary versions, so RunscVersion is
	// used to check compatibility. Version should not be incremented to 1
	// until filesystem checkpoint compatibility is established.
	//
	// TODO: In stable checkpoint formats, require that page size and
	// endianness match between save and restore. Both are fixed on x86, but
	// may vary on arm64. Removing the endianness consistency requirement
	// requires fixing an endianness for tmpfs.fsckptRegularFileSegment, and
	// for pgalloc.MemoryFile save/restore. Removing the page size consistency
	// requirement, specifically adapting to a larger page size after restore,
	// will probably require relocating allocations in the restored MemoryFile.
	Version uint64 `json:"version,omitzero"`

	// RunscVersion is the version of the runsc binary that produced this
	// checkpoint.
	RunscVersion string `json:"runsc_version,omitzero"`

	// MemoryFiles contains information about pgalloc.MemoryFiles stored in the
	// checkpoint, in order of increasing file offsets in the pages metadata
	// and pages files.
	MemoryFiles []MemoryFile `json:"memory_files"`

	// Information about filesystems stored in the checkpoint, in order of
	// increasing file offsets in the multi-tar file.
	Tmpfs []Tmpfs `json:"tmpfs"`
}

// MemoryFile represents a pgalloc.MemoryFile stored in a filesystem
// checkpoint.
type MemoryFile struct {
	// RestoreID is the value of pgalloc.MemoryFile.RestoreID().
	//
	// TODO: Move vfs.RestoreID out of the vfs package, then store it as a
	// struct everywhere rather than sometimes using RestoreID.String().
	RestoreID string `json:"restore_id"`

	// PagesMetadataStart and PagesMetadataEnd are the offsets in the pages
	// metadata file at which the pages metadata for this MemoryFile begin and
	// end respectively.
	PagesMetadataStart uint64 `json:"pages_metadata_start"`
	PagesMetadataEnd   uint64 `json:"pages_metadata_end"`

	// PagesStart and PagesEnd are the offsets in the pages file at which the
	// pages for this MemoryFile begin and end respectively.
	PagesStart uint64 `json:"pages_start"`
	PagesEnd   uint64 `json:"pages_end"`
}

// Tmpfs represents a tmpfs filesystem stored in a filesystem checkpoint.
type Tmpfs struct {
	// MemoryFileRestoreID is the value of this filesystem's
	// pgalloc.MemoryFile.RestoreID().
	//
	// TODO: Assign RestoreIDs to tmpfs filesystems directly so that we can
	// also checkpoint filesystems using the main MemoryFile.
	MemoryFileRestoreID string `json:"memory_file_restore_id"`

	// TarStart and TarEnd are the offsets in the multi-tar file at which the
	// tar archive for this filesystem begin and end respectively.
	TarStart uint64 `json:"tar_start"`
	TarEnd   uint64 `json:"tar_end"`
}
