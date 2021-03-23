# ~~rrply~~ (deprecated and abandoned)
Console player for rockradio.com online radio station.
Made just for fun and personal comfort (avoid registration and advertising).

## End of maintenance

> This project has been moved to [conply](https://github.com/koykov/conply/tree/master/rockradio).

## Preparation
Before building the project you need to have libvlc-dev installed. On Debian/Ubuntu systems please execute:
```bash
apt-get install libvlc-dev
```
ON Centos/RHEL/Fedora systems execute:
```bash
dnf install vlc-devel
```

## Installation
Just execute:
```bash
go get github.com/koykov/rrply
go build -o $GOPATH/bin/rrply github.com/koykov/rrply
```
