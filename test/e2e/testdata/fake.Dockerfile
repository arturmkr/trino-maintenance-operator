FROM scratch
COPY fakebackend /fakebackend
USER 65532:65532
ENTRYPOINT ["/fakebackend"]
