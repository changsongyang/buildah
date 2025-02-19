package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/buildah/copier"
	"github.com/containers/buildah/define"
	"github.com/containers/buildah/internal"
	internalParse "github.com/containers/buildah/internal/parse"
	"github.com/containers/buildah/internal/tmpdir"
	internalUtil "github.com/containers/buildah/internal/util"
	"github.com/containers/common/pkg/parse"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/containers/storage/pkg/unshare"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	selinux "github.com/opencontainers/selinux/go-selinux"
)

const (
	// TypeTmpfs is the type for mounting tmpfs
	TypeTmpfs = "tmpfs"
	// TypeCache is the type for mounting a common persistent cache from host
	TypeCache = "cache"
	// mount=type=cache must create a persistent directory on host so its available for all consecutive builds.
	// Lifecycle of following directory will be inherited from how host machine treats temporary directory
	buildahCacheDir = "buildah-cache"
	// mount=type=cache allows users to lock a cache store while its being used by another build
	BuildahCacheLockfile = "buildah-cache-lockfile"
	// All the lockfiles are stored in a separate directory inside `BuildahCacheDir`
	// Example `/var/tmp/buildah-cache/<target>/buildah-cache-lockfile`
	BuildahCacheLockfileDir = "buildah-cache-lockfiles"
)

var (
	errBadMntOption  = errors.New("invalid mount option")
	errBadOptionArg  = errors.New("must provide an argument for option")
	errBadVolDest    = errors.New("must set volume destination")
	errBadVolSrc     = errors.New("must set volume source")
	errDuplicateDest = errors.New("duplicate mount destination")
)

// CacheParent returns a cache parent for --mount=type=cache
func CacheParent() string {
	return filepath.Join(tmpdir.GetTempDir(), buildahCacheDir+"-"+strconv.Itoa(unshare.GetRootlessUID()))
}

// FIXME: this code needs to be merged with pkg/parse/parse.go ValidateVolumeOpts
// GetBindMount parses a single bind mount entry from the --mount flag.
// Returns specifiedMount and a string which contains name of image that we mounted otherwise its empty.
// Caller is expected to perform unmount of any mounted images
func GetBindMount(ctx *types.SystemContext, args []string, contextDir string, store storage.Store, imageMountLabel string, additionalMountPoints map[string]internal.StageMountDetails, workDir string) (specs.Mount, string, error) {
	newMount := specs.Mount{
		Type: define.TypeBind,
	}

	setRelabel := false
	mountReadability := false
	setDest := false
	bindNonRecursive := false
	fromImage := ""

	for _, val := range args {
		argName, argValue, hasArgValue := strings.Cut(val, "=")
		switch argName {
		case "type":
			// This is already processed
			continue
		case "bind-nonrecursive":
			newMount.Options = append(newMount.Options, "bind")
			bindNonRecursive = true
		case "ro", "nosuid", "nodev", "noexec":
			// TODO: detect duplication of these options.
			// (Is this necessary?)
			newMount.Options = append(newMount.Options, argName)
			mountReadability = true
		case "rw", "readwrite":
			newMount.Options = append(newMount.Options, "rw")
			mountReadability = true
		case "readonly":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
			mountReadability = true
		case "shared", "rshared", "private", "rprivate", "slave", "rslave", "Z", "z", "U", "no-dereference":
			if hasArgValue {
				return newMount, "", fmt.Errorf("%v: %w", val, errBadOptionArg)
			}
			newMount.Options = append(newMount.Options, argName)
		case "from":
			if !hasArgValue {
				return newMount, "", fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			fromImage = argValue
		case "bind-propagation":
			if !hasArgValue {
				return newMount, "", fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			switch argValue {
			default:
				return newMount, "", fmt.Errorf("%v: %q: %w", argName, argValue, errBadMntOption)
			case "shared", "rshared", "private", "rprivate", "slave", "rslave":
				// this should be the relevant parts of the same list of options we accepted above
			}
			newMount.Options = append(newMount.Options, argValue)
		case "src", "source":
			if !hasArgValue {
				return newMount, "", fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			newMount.Source = argValue
		case "target", "dst", "destination":
			if !hasArgValue {
				return newMount, "", fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			targetPath := argValue
			if !path.IsAbs(targetPath) {
				targetPath = filepath.Join(workDir, targetPath)
			}
			if err := parse.ValidateVolumeCtrDir(targetPath); err != nil {
				return newMount, "", err
			}
			newMount.Destination = targetPath
			setDest = true
		case "relabel":
			if setRelabel {
				return newMount, "", fmt.Errorf("cannot pass 'relabel' option more than once: %w", errBadOptionArg)
			}
			setRelabel = true
			switch argValue {
			case "private":
				newMount.Options = append(newMount.Options, "Z")
			case "shared":
				newMount.Options = append(newMount.Options, "z")
			default:
				return newMount, "", fmt.Errorf("%s mount option must be 'private' or 'shared': %w", argName, errBadMntOption)
			}
		case "consistency":
			// Option for OS X only, has no meaning on other platforms
			// and can thus be safely ignored.
			// See also the handling of the equivalent "delegated" and "cached" in ValidateVolumeOpts
		default:
			return newMount, "", fmt.Errorf("%v: %w", argName, errBadMntOption)
		}
	}

	// default mount readability is always readonly
	if !mountReadability {
		newMount.Options = append(newMount.Options, "ro")
	}

	// Following variable ensures that we return imagename only if we did additional mount
	isImageMounted := false
	if fromImage != "" {
		mountPoint := ""
		if additionalMountPoints != nil {
			if val, ok := additionalMountPoints[fromImage]; ok {
				mountPoint = val.MountPoint
			}
		}
		// if mountPoint of image was not found in additionalMap
		// or additionalMap was nil, try mounting image
		if mountPoint == "" {
			image, err := internalUtil.LookupImage(ctx, store, fromImage)
			if err != nil {
				return newMount, "", err
			}

			mountPoint, err = image.Mount(context.Background(), nil, imageMountLabel)
			if err != nil {
				return newMount, "", err
			}
			isImageMounted = true
		}
		contextDir = mountPoint
	}

	// buildkit parity: default bind option must be `rbind`
	// unless specified
	if !bindNonRecursive {
		newMount.Options = append(newMount.Options, "rbind")
	}

	if !setDest {
		return newMount, fromImage, errBadVolDest
	}

	// buildkit parity: support absolute path for sources from current build context
	if contextDir != "" {
		// path should be /contextDir/specified path
		evaluated, err := copier.Eval(contextDir, string(filepath.Separator)+newMount.Source, copier.EvalOptions{})
		if err != nil {
			return newMount, "", err
		}
		newMount.Source = evaluated
	} else {
		// looks like its coming from `build run --mount=type=bind` allow using absolute path
		// error out if no source is set
		if newMount.Source == "" {
			return newMount, "", errBadVolSrc
		}
		if err := parse.ValidateVolumeHostDir(newMount.Source); err != nil {
			return newMount, "", err
		}
	}

	opts, err := parse.ValidateVolumeOpts(newMount.Options)
	if err != nil {
		return newMount, fromImage, err
	}
	newMount.Options = opts

	if !isImageMounted {
		// we don't want any cleanups if image was not mounted explicitly
		// so dont return anything
		fromImage = ""
	}

	return newMount, fromImage, nil
}

// GetCacheMount parses a single cache mount entry from the --mount flag.
//
// If this function succeeds and returns a non-nil *lockfile.LockFile, the caller must unlock it (when??).
func GetCacheMount(args []string, _ storage.Store, _ string, additionalMountPoints map[string]internal.StageMountDetails, workDir string) (specs.Mount, *lockfile.LockFile, error) {
	var err error
	var mode uint64
	var buildahLockFilesDir string
	var (
		setDest           bool
		setShared         bool
		setReadOnly       bool
		foundSElinuxLabel bool
	)
	fromStage := ""
	newMount := specs.Mount{
		Type: define.TypeBind,
	}
	// if id is set a new subdirectory with `id` will be created under /host-temp/buildah-build-cache/id
	id := ""
	// buildkit parity: cache directory defaults to 0o755
	mode = 0o755
	// buildkit parity: cache directory defaults to uid 0 if not specified
	uid := 0
	// buildkit parity: cache directory defaults to gid 0 if not specified
	gid := 0
	// sharing mode
	sharing := "shared"

	for _, val := range args {
		argName, argValue, hasArgValue := strings.Cut(val, "=")
		switch argName {
		case "type":
			// This is already processed
			continue
		case "nosuid", "nodev", "noexec":
			// TODO: detect duplication of these options.
			// (Is this necessary?)
			newMount.Options = append(newMount.Options, argName)
		case "rw", "readwrite":
			newMount.Options = append(newMount.Options, "rw")
		case "readonly", "ro":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
			setReadOnly = true
		case "Z", "z":
			newMount.Options = append(newMount.Options, argName)
			foundSElinuxLabel = true
		case "shared", "rshared", "private", "rprivate", "slave", "rslave", "U":
			newMount.Options = append(newMount.Options, argName)
			setShared = true
		case "sharing":
			sharing = argValue
		case "bind-propagation":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			switch argValue {
			default:
				return newMount, nil, fmt.Errorf("%v: %q: %w", argName, argValue, errBadMntOption)
			case "shared", "rshared", "private", "rprivate", "slave", "rslave":
				// this should be the relevant parts of the same list of options we accepted above
			}
			newMount.Options = append(newMount.Options, argValue)
		case "id":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			id = argValue
		case "from":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			fromStage = argValue
		case "target", "dst", "destination":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			targetPath := argValue
			if !path.IsAbs(targetPath) {
				targetPath = filepath.Join(workDir, targetPath)
			}
			if err := parse.ValidateVolumeCtrDir(targetPath); err != nil {
				return newMount, nil, err
			}
			newMount.Destination = targetPath
			setDest = true
		case "src", "source":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			newMount.Source = argValue
		case "mode":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			mode, err = strconv.ParseUint(argValue, 8, 32)
			if err != nil {
				return newMount, nil, fmt.Errorf("unable to parse cache mode: %w", err)
			}
		case "uid":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			uid, err = strconv.Atoi(argValue)
			if err != nil {
				return newMount, nil, fmt.Errorf("unable to parse cache uid: %w", err)
			}
		case "gid":
			if !hasArgValue {
				return newMount, nil, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			gid, err = strconv.Atoi(argValue)
			if err != nil {
				return newMount, nil, fmt.Errorf("unable to parse cache gid: %w", err)
			}
		default:
			return newMount, nil, fmt.Errorf("%v: %w", argName, errBadMntOption)
		}
	}

	// If selinux is enabled and no selinux option was configured
	// default to `z` i.e shared content label.
	if !foundSElinuxLabel && (selinux.EnforceMode() != selinux.Disabled) && fromStage == "" {
		newMount.Options = append(newMount.Options, "z")
	}

	if !setDest {
		return newMount, nil, errBadVolDest
	}

	if fromStage != "" {
		mountPoint := ""
		if additionalMountPoints != nil {
			if val, ok := additionalMountPoints[fromStage]; ok {
				if !val.IsImage {
					mountPoint = val.MountPoint
				}
			}
		}
		// Cache does not support using an image so if there's no such
		// stage or temporary directory, return an error
		if mountPoint == "" {
			return newMount, nil, fmt.Errorf("no stage or additional build context found with name %s", fromStage)
		}
		// path should be /contextDir/specified path
		evaluated, err := copier.Eval(mountPoint, string(filepath.Separator)+newMount.Source, copier.EvalOptions{})
		if err != nil {
			return newMount, nil, err
		}
		newMount.Source = evaluated
	} else {
		// we need to create the cache directory on the host if no image is being used

		// since type is cache and a cache can be reused by consecutive builds
		// create a common cache directory, which persists on hosts within temp lifecycle
		// add subdirectory if specified

		// cache parent directory: creates separate cache parent for each user.
		cacheParent := CacheParent()
		// create cache on host if not present
		err = os.MkdirAll(cacheParent, os.FileMode(0o755))
		if err != nil {
			return newMount, nil, fmt.Errorf("unable to create build cache directory: %w", err)
		}

		if id != "" {
			// Don't let the user control where we place the directory.
			dirID := digest.FromString(id).Encoded()[:16]
			newMount.Source = filepath.Join(cacheParent, dirID)
			buildahLockFilesDir = filepath.Join(BuildahCacheLockfileDir, dirID)
		} else {
			// Don't let the user control where we place the directory.
			dirID := digest.FromString(newMount.Destination).Encoded()[:16]
			newMount.Source = filepath.Join(cacheParent, dirID)
			buildahLockFilesDir = filepath.Join(BuildahCacheLockfileDir, dirID)
		}
		idPair := idtools.IDPair{
			UID: uid,
			GID: gid,
		}
		// buildkit parity: change uid and gid if specified, otherwise keep `0`
		err = idtools.MkdirAllAndChownNew(newMount.Source, os.FileMode(mode), idPair)
		if err != nil {
			return newMount, nil, fmt.Errorf("unable to change uid,gid of cache directory: %w", err)
		}

		// create a subdirectory inside `cacheParent` just to store lockfiles
		buildahLockFilesDir = filepath.Join(cacheParent, buildahLockFilesDir)
		err = os.MkdirAll(buildahLockFilesDir, os.FileMode(0o700))
		if err != nil {
			return newMount, nil, fmt.Errorf("unable to create build cache lockfiles directory: %w", err)
		}
	}

	var targetLock *lockfile.LockFile // = nil
	succeeded := false
	defer func() {
		if !succeeded && targetLock != nil {
			targetLock.Unlock()
		}
	}()
	switch sharing {
	case "locked":
		// lock parent cache
		lockfile, err := lockfile.GetLockFile(filepath.Join(buildahLockFilesDir, BuildahCacheLockfile))
		if err != nil {
			return newMount, nil, fmt.Errorf("unable to acquire lock when sharing mode is locked: %w", err)
		}
		// Will be unlocked after the RUN step is executed.
		lockfile.Lock()
		targetLock = lockfile
	case "shared":
		// do nothing since default is `shared`
		break
	default:
		// error out for unknown values
		return newMount, nil, fmt.Errorf("unrecognized value %q for field `sharing`: %w", sharing, err)
	}

	// buildkit parity: default sharing should be shared
	// unless specified
	if !setShared {
		newMount.Options = append(newMount.Options, "shared")
	}

	// buildkit parity: cache must writable unless `ro` or `readonly` is configured explicitly
	if !setReadOnly {
		newMount.Options = append(newMount.Options, "rw")
	}

	newMount.Options = append(newMount.Options, "bind")

	opts, err := parse.ValidateVolumeOpts(newMount.Options)
	if err != nil {
		return newMount, nil, err
	}
	newMount.Options = opts

	succeeded = true
	return newMount, targetLock, nil
}

func getVolumeMounts(volumes []string) (map[string]specs.Mount, error) {
	finalVolumeMounts := make(map[string]specs.Mount)

	for _, volume := range volumes {
		volumeMount, err := internalParse.Volume(volume)
		if err != nil {
			return nil, err
		}
		if _, ok := finalVolumeMounts[volumeMount.Destination]; ok {
			return nil, fmt.Errorf("%v: %w", volumeMount.Destination, errDuplicateDest)
		}
		finalVolumeMounts[volumeMount.Destination] = volumeMount
	}
	return finalVolumeMounts, nil
}

// UnlockLockArray is a helper for cleaning up after GetVolumes and the like.
func UnlockLockArray(locks []*lockfile.LockFile) {
	for _, lock := range locks {
		lock.Unlock()
	}
}

// GetVolumes gets the volumes from --volume and --mount
//
// If this function succeeds, the caller must unlock the returned *lockfile.LockFile s if any (when??).
func GetVolumes(ctx *types.SystemContext, store storage.Store, volumes []string, mounts []string, contextDir string, workDir string) ([]specs.Mount, []string, []*lockfile.LockFile, error) {
	unifiedMounts, mountedImages, targetLocks, err := getMounts(ctx, store, mounts, contextDir, workDir)
	if err != nil {
		return nil, mountedImages, nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			UnlockLockArray(targetLocks)
		}
	}()
	volumeMounts, err := getVolumeMounts(volumes)
	if err != nil {
		return nil, mountedImages, nil, err
	}
	for dest, mount := range volumeMounts {
		if _, ok := unifiedMounts[dest]; ok {
			return nil, mountedImages, nil, fmt.Errorf("%v: %w", dest, errDuplicateDest)
		}
		unifiedMounts[dest] = mount
	}

	finalMounts := make([]specs.Mount, 0, len(unifiedMounts))
	for _, mount := range unifiedMounts {
		finalMounts = append(finalMounts, mount)
	}
	succeeded = true
	return finalMounts, mountedImages, targetLocks, nil
}

// getMounts takes user-provided input from the --mount flag and creates OCI
// spec mounts.
// buildah run --mount type=bind,src=/etc/resolv.conf,target=/etc/resolv.conf ...
// buildah run --mount type=cache,target=/var/cache ...
// buildah run --mount type=tmpfs,target=/dev/shm ...
//
// If this function succeeds, the caller must unlock the returned *lockfile.LockFile s if any (when??).
func getMounts(ctx *types.SystemContext, store storage.Store, mounts []string, contextDir string, workDir string) (map[string]specs.Mount, []string, []*lockfile.LockFile, error) {
	// If `type` is not set default to "bind"
	mountType := define.TypeBind
	finalMounts := make(map[string]specs.Mount)
	mountedImages := make([]string, 0)
	targetLocks := make([]*lockfile.LockFile, 0)
	succeeded := false
	defer func() {
		if !succeeded {
			UnlockLockArray(targetLocks)
		}
	}()

	errInvalidSyntax := errors.New("incorrect mount format: should be --mount type=<bind|tmpfs>,[src=<host-dir>,]target=<ctr-dir>[,options]")

	// TODO(vrothberg): the manual parsing can be replaced with a regular expression
	//                  to allow a more robust parsing of the mount format and to give
	//                  precise errors regarding supported format versus supported options.
	for _, mount := range mounts {
		tokens := strings.Split(mount, ",")
		if len(tokens) < 2 {
			return nil, mountedImages, nil, fmt.Errorf("%q: %w", mount, errInvalidSyntax)
		}
		for _, field := range tokens {
			if strings.HasPrefix(field, "type=") {
				kv := strings.Split(field, "=")
				if len(kv) != 2 {
					return nil, mountedImages, nil, fmt.Errorf("%q: %w", mount, errInvalidSyntax)
				}
				mountType = kv[1]
			}
		}
		switch mountType {
		case define.TypeBind:
			mount, image, err := GetBindMount(ctx, tokens, contextDir, store, "", nil, workDir)
			if err != nil {
				return nil, mountedImages, nil, err
			}
			if _, ok := finalMounts[mount.Destination]; ok {
				return nil, mountedImages, nil, fmt.Errorf("%v: %w", mount.Destination, errDuplicateDest)
			}
			finalMounts[mount.Destination] = mount
			mountedImages = append(mountedImages, image)
		case TypeCache:
			mount, tl, err := GetCacheMount(tokens, store, "", nil, workDir)
			if err != nil {
				return nil, mountedImages, nil, err
			}
			if tl != nil {
				targetLocks = append(targetLocks, tl)
			}
			if _, ok := finalMounts[mount.Destination]; ok {
				return nil, mountedImages, nil, fmt.Errorf("%v: %w", mount.Destination, errDuplicateDest)
			}
			finalMounts[mount.Destination] = mount
		case TypeTmpfs:
			mount, err := GetTmpfsMount(tokens, workDir)
			if err != nil {
				return nil, mountedImages, nil, err
			}
			if _, ok := finalMounts[mount.Destination]; ok {
				return nil, mountedImages, nil, fmt.Errorf("%v: %w", mount.Destination, errDuplicateDest)
			}
			finalMounts[mount.Destination] = mount
		default:
			return nil, mountedImages, nil, fmt.Errorf("invalid filesystem type %q", mountType)
		}
	}

	succeeded = true
	return finalMounts, mountedImages, targetLocks, nil
}

// GetTmpfsMount parses a single tmpfs mount entry from the --mount flag
func GetTmpfsMount(args []string, workDir string) (specs.Mount, error) {
	newMount := specs.Mount{
		Type:   TypeTmpfs,
		Source: TypeTmpfs,
	}

	setDest := false

	for _, val := range args {
		argName, argValue, hasArgValue := strings.Cut(val, "=")
		switch argName {
		case "type":
			// This is already processed
			continue
		case "ro", "nosuid", "nodev", "noexec":
			newMount.Options = append(newMount.Options, argName)
		case "readonly":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
		case "tmpcopyup":
			// the path that is shadowed by the tmpfs mount is recursively copied up to the tmpfs itself.
			newMount.Options = append(newMount.Options, argName)
		case "tmpfs-mode":
			if !hasArgValue {
				return newMount, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			newMount.Options = append(newMount.Options, fmt.Sprintf("mode=%s", argValue))
		case "tmpfs-size":
			if !hasArgValue {
				return newMount, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			newMount.Options = append(newMount.Options, fmt.Sprintf("size=%s", argValue))
		case "src", "source":
			return newMount, errors.New("source is not supported with tmpfs mounts")
		case "target", "dst", "destination":
			if !hasArgValue {
				return newMount, fmt.Errorf("%v: %w", argName, errBadOptionArg)
			}
			targetPath := argValue
			if !path.IsAbs(targetPath) {
				targetPath = filepath.Join(workDir, targetPath)
			}
			if err := parse.ValidateVolumeCtrDir(targetPath); err != nil {
				return newMount, err
			}
			newMount.Destination = targetPath
			setDest = true
		default:
			return newMount, fmt.Errorf("%v: %w", argName, errBadMntOption)
		}
	}

	if !setDest {
		return newMount, errBadVolDest
	}

	return newMount, nil
}
