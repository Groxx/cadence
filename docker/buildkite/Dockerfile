FROM golang:1.17

RUN apt-get update && \
  apt-get install -y --no-install-recommends python3 python3-dev python3-setuptools python3-pip \
  && rm -rf /var/lib/apt/lists/*

# only external lib required by cqlsh (and possibly futures)
RUN pip3 install six

# pull cassandra source, trim to enough to run cqlsh, currently only 1MB or so.
RUN mkdir -p cqlsh \
  && cd cqlsh \
  && curl -LO https://archive.apache.org/dist/cassandra/4.0.4/apache-cassandra-4.0.4-src.tar.gz \
  && echo '6c15b0617db5e86550e0b790bda49ee291d4c093  apache-cassandra-4.0.4-src.tar.gz' | shasum -c \
  && tar --strip-components 1 -xvf apache-cassandra-4.0.4-src.tar.gz \
    apache-cassandra-4.0.4-src/bin \
    apache-cassandra-4.0.4-src/lib \
    apache-cassandra-4.0.4-src/pylib \
  && ln -s $(pwd)/bin/cqlsh /usr/local/bin/cqlsh \
  && pwd && ls -alh \
  && rm apache-cassandra-4.0.4-src.tar.gz

RUN cqlsh -h

# verbose test output from `make`, can be disabled with V=0
ENV V=1

# https://github.com/docker-library/golang/blob/c1baf037d71331eb0b8d4c70cff4c29cf124c5e0/1.4/Dockerfile
RUN mkdir -p /cadence
WORKDIR /cadence
