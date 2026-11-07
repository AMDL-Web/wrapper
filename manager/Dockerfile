# Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT

# wrapper-manager for Docker. Run it privileged, or with a seccomp profile
# that allows unshare, and with a volume on /root/data:
#   docker run --privileged -v wrapper-data:/root/data -p 8080:8080 <image> --host 0.0.0.0 --port 8080
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY panel ./panel
RUN CGO_ENABLED=0 go build -trimpath -o /wrapper-manager .

FROM ubuntu:24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /root
COPY --from=build /wrapper-manager /root/wrapper-manager
VOLUME /root/data
EXPOSE 8080
ENTRYPOINT ["/root/wrapper-manager"]
