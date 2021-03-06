#!/bin/bash
set -e
echo "" > coverage.txt
echo $TEST_RACE
os=$(go env GOOS)

if [ "$TEST_PD" = "true" ]; then
  TESTDIRS=$(go list ./... | grep -v vendor)
else
  TESTDIRS=$(go list ./... | grep -v pdserver | grep -v vendor)
fi

echo $TESTDIRS

CGO_CFLAGS="-I${ROCKSDB}/include"
CGO_LDFLAGS="-L${ROCKSDB} -lrocksdb -lstdc++ -lm -lsnappy -ljemalloc"

if [ "$os" == "linux" ]; then
  CGO_LDFLAGS="-L${ROCKSDB} -lrocksdb -lstdc++ -lm -lsnappy -lrt -ljemalloc -ldl"
fi

echo $CGO_LDFLAGS

if [ "$TEST_RACE" = "false" ]; then
    GOMAXPROCS=1 CGO_CFLAGS=${CGO_CFLAGS} CGO_LDFLAGS=${CGO_LDFLAGS} go test -timeout 1500s $TESTDIRS
else
    GOMAXPROCS=4 CGO_CFLAGS=${CGO_CFLAGS} CGO_LDFLAGS=${CGO_LDFLAGS} go test -timeout 1500s -race $TESTDIRS
    for d in $TESTDIRS; do 
        GOMAXPROCS=4 CGO_CFLAGS=${CGO_CFLAGS} CGO_LDFLAGS=${CGO_LDFLAGS} go test -timeout 1500s -race -coverprofile=profile.out -covermode=atomic $d
        if [ -f profile.out ]; then
            cat profile.out >> coverage.txt
            rm profile.out
        fi
    done
fi

# no tests, but a build is something
for dir in $(find apps tools -maxdepth 1 -type d) ; do
    if grep -q '^package main$' $dir/*.go 2>/dev/null; then
        echo "building $dir"
        CGO_CFLAGS=${CGO_CFLAGS} CGO_LDFLAGS=${CGO_LDFLAGS} go build -o $dir/$(basename $dir) ./$dir
    else
        echo "(skipped $dir)"
    fi
done
