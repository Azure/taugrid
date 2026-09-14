# NCCL tests diagnostic image

This directory owns the image used only by the manually gated
`NCCL/RDMA 2x8xH200` e2e diagnostic. It is not a Tau workload image or a
standard Tau profile.

`versions.json` is the authoritative source for component build arguments, and
the Makefile supplies those same values to local and CI builds. The digest-pinned
MCR base has one authoritative declaration: the Dockerfile `FROM` instruction,
where Dependabot can discover and update it. The Makefile and CI do not override
or duplicate that base. The build uses an amd64 AzureML CUDA development base.
The base contract pins CUDA 12.4.1, NCCL 2.21.5, and Open MPI 5.0.6; Ubuntu
Jammy's rdma-core packages are pinned to 39.0-1; and nccl-tests 2.16.0 is pinned
by commit and archive SHA-256. The upstream nccl-tests BSD-3-Clause license is
copied into the image.

```bash
make -C images/nccl-tests docker-build
make -C images/nccl-tests test
```

Do not publish this image from contributor or diagnostic work. A repository
image producer may publish it through an approved release process. Before a
live diagnostic, an operator must resolve the approved image to an immutable
`mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:<64 lowercase hex>`
reference and provide that digest to the harness. Other repositories and tags
are rejected.

The worker entrypoint raises the memlock limit before starting `sshd` on port
2222. The MPIJob drops all capabilities, then adds only the memlock capabilities
`IPC_LOCK` and `SYS_RESOURCE` plus OpenSSH's required privilege-separation
capabilities `SETGID`, `SETUID`, and `SYS_CHROOT`. It disables privilege
escalation and uses RuntimeDefault seccomp. `make test` performs an actual
public-key SSH authentication and remote-command smoke with exactly that
capability set; starting sshd alone is not considered sufficient validation.
This validates SSH authentication and privilege separation only. Effective
memlock behavior with the target Kubernetes runtime and NCCL over injected RDMA
devices remain pending an authorized live diagnostic.
