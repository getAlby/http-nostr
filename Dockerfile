FROM golang:1.21-alpine as builder

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

# Copy the binaries and entrypoint from the builder image.
COPY --from=builder /build/main /bin/
COPY --from=builder /build/daily-cleanup /bin/

ENTRYPOINT [ "/bin/main" ]
