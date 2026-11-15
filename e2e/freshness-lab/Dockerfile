FROM debian:13
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 gnupg dpkg-dev apt-utils ca-certificates && \
    rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/* /etc/apt/sources.list
COPY lab.py /lab/lab.py
ENTRYPOINT ["python3", "/lab/lab.py"]
