# wrapper-manager with the wrapper-lite payload baked in. A Deploy to
# Cloudflare copy holds only this directory, so the manager source is fetched
# at WRAPPER_COMMIT, the commit of AMDL-Web/wrapper this template was tested
# with. Bump it together with any template change that needs a newer manager.
FROM golang:1.25 AS build
ARG WRAPPER_COMMIT=7dd65615a7de963875135d5aa7bade2e76e67540
WORKDIR /src
RUN git init -q . \
    && git fetch -q --depth 1 https://github.com/AMDL-Web/wrapper "$WRAPPER_COMMIT" \
    && git checkout -q FETCH_HEAD
WORKDIR /src/manager
RUN CGO_ENABLED=0 go build -o /wrapper-manager .

FROM ubuntu:24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /root
COPY --from=build /wrapper-manager ./
# Empty: the wrapper-lite release the manager pins. Set to install another
# payload zip.
ENV LITE_PAYLOAD_URL=
RUN ./wrapper-manager -prepare ${LITE_PAYLOAD_URL:+-payload-url "$LITE_PAYLOAD_URL"}
EXPOSE 8080 8081
# 8080 is the API the Worker proxies; 8081 is the admin API (snapshot,
# restore, stats), which only the Durable Object reaches.
ENTRYPOINT ["/bin/sh", "-c", "exec ./wrapper-manager -host 0.0.0.0 -port 8080 -admin-port 8081 -launcher root ${LITE_PAYLOAD_URL:+-payload-url \"$LITE_PAYLOAD_URL\"}"]
