FROM scratch
COPY rootfs/ /
ENTRYPOINT ["/argonaut"]
