CGO_ENABLED=1 go build -ldflags="-extldflags=-static" -tags sqlite_omit_load_extension,netgo
#CGO_ENABLED=0 go build
