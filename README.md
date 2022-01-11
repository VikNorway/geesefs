<img src="doc/geesefs.png" height="64" width="64" align="middle" />

GeeseFS is a high-performance, POSIX-ish S3
([Yandex](https://cloud.yandex.com/en-ru/services/storage), [Amazon](https://aws.amazon.com/s3/))
file system written in Go

# Overview

GeeseFS allows you to mount an S3 bucket as a file system.

FUSE file systems based on S3 typically have performance problems, especially with small files and metadata operations.

GeeseFS attempts to solve these problems by using aggressive parallelism and asynchrony.

Also check out our CSI S3 driver (GeeseFS-based): https://github.com/yandex-cloud/csi-s3

# POSIX Compatibility Matrix

|                   | GeeseFS | rclone | Goofys | S3FS | gcsfuse |
| ----------------- | ------- | ------ | ------ | ---- | ------- |
| Read after write  |    +    |    +   |    -   |   +  |    +    |
| Partial writes    |    +    |    +   |    -   |   +  |    +    |
| Truncate          |    +    |    -   |    -   |   +  |    +    |
| chmod/chown       |    -    |    -   |    -   |   +  |    -    |
| fsync             |    +    |    -   |    -   |   +  |    +    |
| Symlinks          |    +    |    -   |    -   |   +  |    +    |
| xattr             |    +    |    -   |    +   |   +  |    -    |
| Directory renames |    +    |    +   |    *   |   +  |    +    |
| readdir & changes |    +    |    +   |    -   |   +  |    +    |

\* Directory renames are allowed in Goofys for directories with no more than 1000 entries and the limit is hardcoded

List of non-POSIX behaviors/limitations for GeeseFS:
* symbolic links are only restored correctly when using Yandex S3 because standard S3
  doesn't return user metadata in listings and detecting symlinks in standard S3 would
  require an additional HEAD request for every file in listing which would make listings
  too slow
* does not store file mode/owner/group, use `--(dir|file)-mode` or `--(uid|gid)` options
* does not support hard links
* does not support special files (block/character devices, named pipes, UNIX sockets)
* does not support locking
* `ctime`, `atime` is always the same as `mtime`
* file modification time can't be set by user (for example with `cp --preserve` or utimes(2))

In addition to the items above:
* default file size limit is 1.03 TB, achieved by splitting the file into 1000x 5MB parts,
  1000x 25 MB parts and 8000x 125 MB parts. You can change part sizes, but AWS's own limit
  is anyway 5 TB.

Owner & group, modification times and special files are in fact supportable with Yandex S3
because it has listings with metadata. Feel free to post issues if you want it. :-)

# Stability

GeeseFS is stable enough to pass most of `xfstests` which are applicable,
including dirstress/fsstress stress-tests (generic/007, generic/011, generic/013).

See also [#Common Issues](#Common Issues).

# Performance Features

|                                | GeeseFS | rclone | Goofys | S3FS | gcsfuse |
| ------------------------------ | ------- | ------ | ------ | ---- | ------- |
| Parallel readahead             |    +    |    -   |    +   |   +  |    -    |
| Parallel multipart uploads     |    +    |    -   |    +   |   +  |    -    |
| No readahead on random read    |    +    |    -   |    +   |   -  |    +    |
| Server-side copy on append     |    +    |    -   |    -   |   *  |    +    |
| Server-side copy on update     |    +    |    -   |    -   |   *  |    -    |
| xattrs without extra RTT       |    +*   |    -   |    -   |   -  |    +    |
| Fast recursive listings        |    +    |    -   |    *   |   -  |    +    |
| Asynchronous write             |    +    |    +   |    -   |   -  |    -    |
| Asynchronous delete            |    +    |    -   |    -   |   -  |    -    |
| Asynchronous rename            |    +    |    -   |    -   |   -  |    -    |
| Disk cache for reads           |    +    |    *   |    -   |   +  |    +    |
| Disk cache for writes          |    +    |    *   |    -   |   +  |    -    |

\* Recursive listing optimisation in Goofys is buggy and may skip files under certain conditions

\* S3FS uses server-side copy, but it still downloads the whole file to update it. And it's buggy too :-)

\* rclone mount has VFS cache, but it can only cache whole files. And it's also buggy - it often hangs on write.

\* xattrs without extra RTT only work with Yandex S3 (--list-type=ext-v1).

# Installation

* Pre-built binaries:
  * [Linux amd64](https://github.com/yandex-cloud/geesefs/releases/latest/download/geesefs-linux-amd64). You may also need to install fuse-utils first.
  * [Mac amd64](https://github.com/yandex-cloud/geesefs/releases/latest/download/geesefs-mac-amd64),
    [arm64](https://github.com/yandex-cloud/geesefs/releases/latest/download/geesefs-mac-arm64). You also need osxfuse/macfuse for GeeseFS to work.
* Or build from source with Go 1.13 or later:

```ShellSession
$ go get github.com/yandex-cloud/geesefs
```

# Usage

```ShellSession
$ cat ~/.aws/credentials
[default]
aws_access_key_id = AKID1234567890
aws_secret_access_key = MY-SECRET-KEY
$ $GOPATH/bin/geesefs <bucket> <mountpoint>
$ $GOPATH/bin/geesefs [--endpoint https://...] <bucket:prefix> <mountpoint> # if you only want to mount objects under a prefix
```

You can also supply credentials via the `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` environment variables.

To mount an S3 bucket on startup make sure the credential is
configured for `root` and add this to `/etc/fstab`:

```
bucket    /mnt/mountpoint    fuse.geesefs    _netdev,allow_other,--file-mode=0666,--dir-mode=0777    0    0
```

You can also use a different path to the credentials file by adding `,--shared-config=/path/to/credentials`.

See also: [Instruction for Azure Blob Storage](https://github.com/yandex-cloud/geesefs/blob/master/README-azure.md).

# Benchmarks

See [bench/README.md](bench/README.md).

# Configuration

There's a lot of tuning you can do. Consult `geesefs -h` to view the list of options.

# Common Issues

## Memory Limit

Default internal cache memory limit in GeeseFS (--memory-limit) is 1 GB. GeeseFS uses cache
for read buffers when it needs to load data from the server. At the same time, default "large"
readahead setting is configured to be 100 MB which is optimal for linear read performance.

However, that means that more than 10 processes trying to read large files at the same time
may exceed that memory limit by requesting more than 1000 MB of buffers and in that case
GeeseFS will return ENOMEM errors to some of them.

You can overcome this problem by either raising --memory-limit (for example to 4 GB)
or lowering --read-ahead-large (for example to 20 MB).

## Batch Forget

The following error message in logs is harmless and occurs regularly:

`fuse.ERROR writeMessage: no such file or directory [16 0 0 0 218 255 255 255 176 95 0 0 0 0 0 0]`

It is caused by the lack of FUSE BatchForget operation implementation and kernel returning ENOENT
when GeeseFS tries to tell it that BatchForget isn't implemented :-)

It doesn't lead to any problems because the kernel sends additional non-batch Forget operations
after an unsuccessful BatchForget.

BatchForget will be implemented at some point in the future and then the error message will stop
appearing.

## Concurrent Updates

GeeseFS doesn't support concurrent updates of the same file from multiple hosts. If you try to
do that you should guarantee that one host calls `fsync()` on the modified file and then waits
for at least `--stat-cache-ttl` (1 minute by default) before allowing other hosts to start
updating the file. If you don't do that you may encounter lost updates (conflicts) which are
reported in the log in the following way:

```
main.WARNING File xxx/yyy is deleted or resized remotely, discarding local changes
```

## Troubleshooting

If you experience any problems with GeeseFS - if it crashes, hangs or does something else nasty:

- Update to the latest version if you haven't already done it
- Check your system log (syslog/journalctl) and dmesg for error messages from GeeseFS
- Try to start GeeseFS in debug mode: `--debug_s3 --debug_fuse --log-file /path/to/log.txt`,
  reproduce the problem and send it to us via Issues or any other means.

# License

Licensed under the Apache License, Version 2.0

See `LICENSE` and `AUTHORS`

## Compatibility with S3

geesefs works with:
* Yandex Object Storage (default)
* Amazon S3
* Ceph (and also Ceph-based Digital Ocean Spaces, DreamObjects, gridscale etc)
* Minio
* OpenStack Swift
* Azure Blob Storage (even though it's not S3)
* Backblaze B2

It should also work with any other S3 that implements multipart uploads and
multipart server-side copy (UploadPartCopy).

Important note: you should mount geesefs with `--list-type 2` or `--list-type 1` options
if you use it with non-Yandex S3.

The following backends are inherited from Goofys code and still exist, but are broken:
* Google Cloud Storage
* Azure Data Lake Gen1
* Azure Data Lake Gen2

# References

* [Yandex Object Storage](https://cloud.yandex.com/en-ru/services/storage)
* [Amazon S3](https://aws.amazon.com/s3/)
* [Amazon SDK for Go](https://github.com/aws/aws-sdk-go)
* Other related fuse filesystems
  * [goofys](https://github.com/kahing/goofys): GeeseFS is a fork of Goofys.
  * [s3fs](https://github.com/s3fs-fuse/s3fs-fuse): another popular filesystem for S3.
  * [gcsfuse](https://github.com/googlecloudplatform/gcsfuse): filesystem for
    [Google Cloud Storage](https://cloud.google.com/storage/). Goofys
    borrowed some skeleton code from this project.
* [S3Proxy](https://github.com/andrewgaul/s3proxy)
* [fuse binding](https://github.com/jacobsa/fuse), also used by `gcsfuse`
