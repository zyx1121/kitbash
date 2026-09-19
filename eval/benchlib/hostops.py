"""What the harness does to the host itself: ssh as root, and ssh as a member.

A fault is injected here and nowhere else. The agent never sees any of it; it
sees a Process that stopped answering, which is what a user sees.
"""

import os
import subprocess


def ssh(alias, command, timeout=300, check=False):
    """Run one command over ssh. Answers (code, stdout, stderr)."""
    result = subprocess.run(
        ["ssh", "-o", "BatchMode=yes", alias, command],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if check and result.returncode:
        raise RuntimeError("%s on %s: %s" % (command, alias, result.stderr.strip()[:400]))
    return result.returncode, result.stdout, result.stderr


def as_member(root_alias, member, uid, command, timeout=300):
    """Run one command as a member, with the runtime directory their podman needs."""
    quoted = command.replace("'", "'\\''")
    wrapped = "su - %s -c 'XDG_RUNTIME_DIR=/run/user/%s %s'" % (member, uid, quoted)
    return ssh(root_alias, wrapped, timeout=timeout)


def kitbash_version(root_alias):
    """The version of the kitbashd on the host, from its own package record."""
    code, out, _ = ssh(root_alias, "apk list -I kitbashd 2>/dev/null | head -1")
    first = (out or "").strip().split(" ")[0]
    if first.startswith("kitbashd-"):
        return first[len("kitbashd-"):].rsplit("-r", 1)[0]
    code, out, _ = ssh(root_alias, "kitbashd --version 2>/dev/null | head -1")
    return (out or "").strip().lstrip("v") or "unknown"


def domain(root_alias):
    """The domain every http Process of this host is served under."""
    code, out, _ = ssh(root_alias, ". /etc/conf.d/kitbashd; printf '%s' \"$KITBASH_DOMAIN\"")
    return (out or "").strip()


def resolved(alias):
    """What ssh makes of an alias, so the round's own config can reach the same host."""
    result = subprocess.run(["ssh", "-G", alias], capture_output=True, text=True, check=True)
    values = {}
    for line in result.stdout.splitlines():
        parts = line.split(" ", 1)
        if len(parts) != 2:
            continue
        if parts[0] == "identityfile":
            values["identityfile"] = (values.get("identityfile", "") + " " + parts[1]).strip()
        else:
            values.setdefault(parts[0], parts[1])
    return values


def write_ssh_config(path, host_alias, member, key_path, alias="bench-member"):
    """An ssh config the harness and the agent both use to reach the member.

    Everything the connection needs is written out, the jump host included,
    because a ProxyJump that names an alias is resolved by a second ssh that
    does not read this file.
    """
    values = resolved(host_alias)
    lines = [
        "# Written by eval/bench.py for one round. The key it names is the round's own.",
        "Host %s" % alias,
        "  HostName %s" % values.get("hostname"),
        "  Port %s" % values.get("port", "22"),
        "  User %s" % member,
        "  IdentityFile %s" % key_path,
        "  IdentitiesOnly yes",
        "  StrictHostKeyChecking accept-new",
        "  UserKnownHostsFile %s" % os.path.join(os.path.dirname(path), "known_hosts"),
        "  BatchMode yes",
        "  ServerAliveInterval 30",
    ]
    jump = values.get("proxyjump")
    if jump and jump != "none":
        lines.append("  ProxyCommand ssh -F %s -W %%h:%%p %s-jump" % (path, alias))
        jump_values = resolved(jump)
        lines += [
            "",
            "Host %s-jump" % alias,
            "  HostName %s" % jump_values.get("hostname", jump),
            "  Port %s" % jump_values.get("port", "22"),
            "  User %s" % jump_values.get("user", "root"),
            "  StrictHostKeyChecking accept-new",
            "  UserKnownHostsFile %s" % os.path.join(os.path.dirname(path), "known_hosts"),
            "  BatchMode yes",
        ]
        for identity in jump_values.get("identityfile", "").split():
            if identity:
                lines.append("  IdentityFile %s" % os.path.expanduser(identity))
    lines.append("")
    with open(path, "w") as handle:
        handle.write("\n".join(lines))
    os.chmod(path, 0o600)
    return alias


def generate_key(path, comment):
    """A fresh ed25519 key for the round's member, 0600, deleted at teardown."""
    if os.path.exists(path):
        os.remove(path)
    if os.path.exists(path + ".pub"):
        os.remove(path + ".pub")
    subprocess.run(
        ["ssh-keygen", "-t", "ed25519", "-N", "", "-C", comment, "-f", path],
        capture_output=True,
        check=True,
    )
    os.chmod(path, 0o600)
    with open(path + ".pub") as handle:
        return handle.read().strip()
