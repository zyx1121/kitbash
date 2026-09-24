"""What the harness does to the host itself: ssh as root, and ssh as a member.

A fault is injected here and nowhere else. The agent never sees any of it; it
sees a Process that stopped answering, which is what a user sees.
"""

import os
import re
import shlex
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


# A member's name as kitbash spells it, which is what a home under /home is.
MEMBER = re.compile(r"^[a-z][a-z0-9-]*$")


class UnsafePath(Exception):
    """A path the bench would change as root that is not plainly the member's own."""


def member_path(member, path):
    """The path, if it is written plainly below the member's home, or UnsafePath.

    Plainly: absolute, normalized, no dot components, below /home/<member>/.
    Whether a link on the host makes it resolve anywhere else is what
    guard() asks there, because only the host can say.
    """
    if not MEMBER.match(member or ""):
        raise UnsafePath("%r is not a member name" % member)
    home = "/home/%s" % member
    if not path.startswith(home + "/") or os.path.normpath(path) != path or "/." in path:
        raise UnsafePath("%s is not written plainly below %s" % (path, home))
    return path


def guard(member, path, kind="-f"):
    """Shell that succeeds only if the path is a regular file (or with -d a
    directory), is not a symbolic link, and resolves to itself, so no
    component of it is a link, below the member's home.

    Every root command on a member's path runs behind this. A member, or a
    Process of theirs, can put a link where the bench expects a file, and a
    root command that follows it would change a file of root's choosing.
    """
    home = "/home/%s" % member
    quoted = shlex.quote(path)
    return ('[ %s %s ] && [ ! -L %s ] && [ "$(readlink -f %s)" = %s ] && case %s in %s|%s/*) true ;; *) false ;; esac'
            % (kind, quoted, quoted, quoted, quoted, quoted, shlex.quote(home), shlex.quote(home)))


def make_immutable(root_alias, member, uid, path, create="[]"):
    """A file of the member's made unwritable at the inode, the mount fault.

    Everything the member may do is done as the member: creating the file
    when it is missing, taking it back from a container's user with podman
    unshare, and the mode. Only the immutable flag needs root. Every step,
    the member's too, runs behind guard(), so a link planted at the path or
    at its folder is refused rather than followed. Answers (code, stdout,
    stderr) of the last step run.
    """
    member_path(member, path)
    quoted = shlex.quote(path)
    refuse = "{ echo 'refused: %s is not a regular file of %s that no link leads to' >&2; exit 1; }" % (path, member)
    own = ("if [ ! -e %s ] && [ ! -L %s ]; then %s && printf '%%s\\n' %s > %s; fi; "
           "%s || %s; podman unshare chown -h 0:0 %s 2>/dev/null; chmod 0444 %s"
           % (quoted, quoted, guard(member, os.path.dirname(path), "-d"), shlex.quote(create), quoted,
              guard(member, path), refuse, quoted, quoted))
    code, out, err = as_member(root_alias, member, uid, "sh -c %s" % shlex.quote(own))
    if code:
        return code, out, err
    return ssh(root_alias, "%s || %s; chattr +i %s" % (guard(member, path), refuse, quoted))


def clear_immutable(root_alias, member, uid, path):
    """The mount fault taken back: the flag cleared as root and the mode as the member, both behind guard()."""
    member_path(member, path)
    quoted = shlex.quote(path)
    code, out, err = ssh(root_alias, "%s && chattr -i %s; true" % (guard(member, path), quoted))
    as_member(root_alias, member, uid,
              "sh -c %s" % shlex.quote("%s && chmod 0644 %s; true" % (guard(member, path), quoted)))
    return code, out, err


def clear_home_flags(root_alias, member):
    """Every immutable flag under a member's home cleared, before users_remove archives it.

    chattr -R does not follow the links it meets below the top, and the top
    is behind guard(), so the home itself cannot be a link to elsewhere.
    """
    if not MEMBER.match(member or ""):
        raise UnsafePath("%r is not a member name" % member)
    home = "/home/%s" % member
    return ssh(root_alias, "%s && chattr -R -i %s 2>/dev/null; true" % (guard(member, home, "-d"), shlex.quote(home)))


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
