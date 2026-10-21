FROM golang:1.26.10-alpine as builder

# Move to working directory /build
WORKDIR /build

# Copy and download dependency using go mod
COPY go.mod .
COPY go.sum .
RUN go mod download

# Copy the code into the container
COPY . .

# Build the application
RUN go build -o main cmd/server/main.go

# Build the utility scripts
RUN go build -o daily-cleanup cmd/daily-cleanup/main.go

# Start a new, final image to reduce size.
FROM alpine as final

# Always pull the latest CA bundle (e.g. the eMudhra compromise, Oct 2026,
# removed "emSign ECC Root CA - G3" and "AAA Certificate Services") and fail
# the build if those roots are still trusted.
RUN apk upgrade --no-cache ca-certificates-bundle && \
  apk add --no-cache --virtual .ca-check openssl && \
  apk info -v ca-certificates-bundle && \
  ! openssl crl2pkcs7 -nocrl -certfile /etc/ssl/certs/ca-certificates.crt | \
    openssl pkcs7 -print_certs -noout | \
    grep -E "CN ?= ?(emSign ECC Root CA - G3|AAA Certificate Services)$" && \
  apk del .ca-check

# Copy the binaries and entrypoint from the builder image.
COPY --from=builder /build/main /bin/
COPY --from=builder /build/daily-cleanup /bin/

ENTRYPOINT [ "/bin/main" ]
