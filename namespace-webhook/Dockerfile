# Use a lightweight Go image
FROM golang:1.22.5 AS build

ENV GOOS=linux
ENV GOARCH=amd64
ENV CGO_ENABLED=0

# Set the working directory
WORKDIR /app

# Copy the Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code and build the application
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build,sharing=private \
  go build -o bin/admission-webhook .

# Use a minimal image for the final build
FROM scratch AS run

COPY --from=build /app/bin/admission-webhook /usr/local/bin/

# Set entrypoint
CMD ["admission-webhook"]
