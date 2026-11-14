FROM infisical/cli:0.45.8@sha256:ef5cd9bef770e1d13acfa18414a65719a262c4b7681735086416852f1574d019 AS cli
FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq python3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=cli /bin/infisical /usr/local/bin/infisical
COPY github-user /usr/local/bin/github-user
RUN chmod 755 /usr/local/bin/github-user
WORKDIR /workspace
