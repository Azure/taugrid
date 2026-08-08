package sourcebundle

const (
	InitEnvSourcePath = "TAU_SOURCE_BUNDLE_PATH"
	InitEnvDigest     = "TAU_SOURCE_BUNDLE_DIGEST"
	InitEnvTargetDir  = "TAU_SOURCE_BUNDLE_TARGET_DIR"
	InitEnvMode       = "TAU_SOURCE_BUNDLE_MODE"
)

// InitScript is a standard-library-only Python contract for bundle validation
// and extraction. Renderers pass it and the four InitEnv* values to workloads.
func InitScript() string { return pythonInitScript }

const pythonInitScript = `import hashlib
import os
import pathlib
import posixpath
import stat
import sys
import zipfile

SOURCE = os.environ.get("TAU_SOURCE_BUNDLE_PATH", "")
DIGEST = os.environ.get("TAU_SOURCE_BUNDLE_DIGEST", "")
TARGET = os.environ.get("TAU_SOURCE_BUNDLE_TARGET_DIR", "")
MODE = os.environ.get("TAU_SOURCE_BUNDLE_MODE", "")
MAX_ENTRIES = 4096
MAX_EXPANDED_BYTES = 64 * 1024 * 1024

def fail(message):
    raise RuntimeError("source bundle: " + message)

if not SOURCE or not DIGEST or not TARGET or MODE not in ("validate", "extract"):
    fail("TAU_SOURCE_BUNDLE_PATH, TAU_SOURCE_BUNDLE_DIGEST, TAU_SOURCE_BUNDLE_TARGET_DIR, and TAU_SOURCE_BUNDLE_MODE=validate|extract are required")
if not DIGEST.startswith("sha256:") or len(DIGEST) != 71:
    fail("digest must be sha256:<64 hex>")
try:
    int(DIGEST[7:], 16)
except ValueError:
    fail("digest must be sha256:<64 hex>")
if DIGEST[7:] != DIGEST[7:].lower():
    fail("digest must use lowercase hex")

hasher = hashlib.sha256()
try:
    with open(SOURCE, "rb") as source_file:
        for chunk in iter(lambda: source_file.read(1024 * 1024), b""):
            hasher.update(chunk)
except OSError as error:
    fail("cannot read archive: " + str(error))
if hasher.hexdigest() != DIGEST[7:]:
    fail("archive sha256 does not match expected digest")

def inspect(info, seen, total):
    name = info.filename
    if not name or "\x00" in name or "\\" in name or name.startswith("/"):
        fail("unsafe zip member name: " + repr(name))
    normalized = posixpath.normpath(name)
    if normalized in ("", ".", "..") or normalized.startswith("../") or normalized.startswith("/"):
        fail("unsafe zip member name: " + repr(name))
    if normalized in seen:
        fail("duplicate normalized zip member: " + repr(normalized))
    seen.add(normalized)
    mode = (info.external_attr >> 16) & 0xffff
    if stat.S_ISLNK(mode):
        fail("symlink zip member: " + repr(name))
    if info.is_dir():
        if mode and stat.S_IFMT(mode) not in (0, stat.S_IFDIR):
            fail("special zip member: " + repr(name))
    elif mode and stat.S_IFMT(mode) not in (0, stat.S_IFREG):
        fail("special zip member: " + repr(name))
    total += info.file_size
    if total > MAX_EXPANDED_BYTES:
        fail("expanded archive exceeds byte limit")
    return normalized, total

try:
    archive = zipfile.ZipFile(SOURCE)
    infos = archive.infolist()
    if len(infos) > MAX_ENTRIES:
        fail("archive exceeds entry limit")
    seen, total, members = set(), 0, []
    for info in infos:
        normalized, total = inspect(info, seen, total)
        members.append((info, normalized))
    target = pathlib.Path(TARGET).resolve()
    if MODE == "extract":
        target.mkdir(parents=True, exist_ok=True)
    for info, normalized in members:
        destination = (target / pathlib.PurePosixPath(normalized)).resolve()
        if not destination.is_relative_to(target):
            fail("zip member escapes extraction target")
        if info.is_dir():
            if MODE == "extract":
                destination.mkdir(parents=True, exist_ok=True)
            continue
        if MODE == "extract":
            destination.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(info, "r") as input_file, open(destination, "xb") as output_file:
                for chunk in iter(lambda: input_file.read(1024 * 1024), b""):
                    output_file.write(chunk)
            member_mode = (info.external_attr >> 16) & 0xffff
            os.chmod(destination, 0o755 if member_mode & 0o111 else 0o644)
        else:
            with archive.open(info, "r") as input_file:
                for chunk in iter(lambda: input_file.read(1024 * 1024), b""):
                    pass
except (OSError, zipfile.BadZipFile, RuntimeError) as error:
    print("source bundle validation failed: " + str(error), file=sys.stderr)
    sys.exit(1)
finally:
    try:
        archive.close()
    except NameError:
        pass
`
