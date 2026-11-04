"""Download one model and report how far it got (DURO-184). The oven runs this in the download Job.

Runs `hf download` as before and, next to it, writes one JSON line to stdout every 5 seconds:

    {"event": "download_progress", "downloadedBytes": 123, "totalBytes": 456}

totalBytes is null while the size on the Hub is unknown. The oven reads the last such line from the Pod log. The exit code
is the one of `hf download`: nothing about progress may fail, slow down or retry the download. HF_TOKEN goes to the Hub and
to `hf`; the lines hold numbers only.

Where the bytes are, as measured with huggingface_hub 2.2.0 and hf_xet 1.7.0 (the vllm-openai v0.32.0 image):

    hub/models--org--name/blobs/<etag>                   finished file, or a symlink to the shared store
    hub/models--org--name/blobs/<etag>.<id>.incomplete   the file being written; it grows while downloading
    hub/blobs/<2 hex>/<xet hash>                         shared store: a finished Xet file moves here

So the model's blobs folder has to be read through its symlinks: its own size stops growing once a file moves to the
store. Every attempt writes to its own .incomplete name, and a killed attempt leaves its file behind, so one etag counts
once. Older versions keep `<etag>.incomplete` and regular files, which the same reading covers.

Needs python3 and the hf CLI, both in the vllm/vllm-openai image. Standard library only.
"""

import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request

EVENT = "download_progress"
INTERVAL_SECONDS = 5.0
TOTAL_RETRY_SECONDS = 60.0
API_TIMEOUT_SECONDS = 15.0
# Waits for the last line after `hf` is done. The exit code must not wait for a slow Hub or a stuck log.
LAST_LINE_WAIT_SECONDS = 2.0
EXIT_NO_HF = 127


def blobs_dir(env, model):
    cache = env.get("HF_HUB_CACHE") or os.path.join(env.get("HF_HOME", "/models"), "hub")
    return os.path.join(cache, "models--" + model.replace("/", "--"), "blobs")


def downloaded_bytes(blobs):
    """Bytes of this model on disk: a finished file counts in full, otherwise the longest partial file of that etag."""
    finished, partial = {}, {}
    try:
        entries = os.listdir(blobs)
    except OSError:
        return 0  # Nothing written yet, or the volume hiccuped. The next look tells.
    for name in entries:
        try:
            size = os.stat(os.path.join(blobs, name)).st_size  # follows the symlink into the shared store
        except OSError:
            continue  # Moved between listing and stat, or a dangling link
        etag = name.split(".", 1)[0]
        if name.endswith(".incomplete"):
            partial[etag] = max(partial.get(etag, 0), size)
        else:
            finished[etag] = size
    return sum(finished.values()) + sum(size for etag, size in partial.items() if etag not in finished)


def repo_size(siblings):
    """Sum of the files `hf download` will store. Files with the same content are one blob. None when a size is missing."""
    blobs = {}
    for sibling in siblings:
        lfs = sibling.get("lfs") or {}
        size = lfs.get("size", sibling.get("size"))
        if not isinstance(size, int) or size < 0:
            return None
        blobs[lfs.get("sha256") or sibling.get("blobId") or sibling.get("rfilename")] = size
    return sum(blobs.values()) or None


def fetch_total(model, env, timeout=API_TIMEOUT_SECONDS):
    """Size of the model on the Hub, or None. Standard library, so a --download-image whose python3 lacks huggingface_hub works."""
    endpoint = (env.get("HF_ENDPOINT") or "https://huggingface.co").rstrip("/")
    request = urllib.request.Request(f"{endpoint}/api/models/{urllib.parse.quote(model)}?blobs=true")
    token = env.get("HF_TOKEN")
    if token:
        # Unredirected: urllib would otherwise hand the token to wherever a redirect points.
        request.add_unredirected_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return repo_size(json.load(response)["siblings"])


class Reporter(threading.Thread):
    """Writes the progress lines. Whatever goes wrong here costs one line, never the download."""

    def __init__(self, model, env, out=None, interval=INTERVAL_SECONDS, total_retry=TOTAL_RETRY_SECONDS):
        super().__init__(daemon=True)
        self.model, self.env = model, env
        self.blobs = blobs_dir(env, model)
        self.out = out or sys.stdout
        self.interval, self.total_retry = interval, total_retry
        self.total, self.next_total_try = None, 0.0
        self.stopping = threading.Event()

    def look_up_total(self):
        if self.total is not None or time.monotonic() < self.next_total_try:
            return
        self.next_total_try = time.monotonic() + self.total_retry
        try:
            self.total = fetch_total(self.model, self.env)
        except Exception as error:  # noqa: BLE001 - see the class comment. The type only: the message may hold a URL or a header.
            print(f"download: size on the Hub unknown for now ({type(error).__name__})", file=sys.stderr, flush=True)

    def write_line(self):
        line = {"event": EVENT, "downloadedBytes": downloaded_bytes(self.blobs), "totalBytes": self.total}
        self.out.write(json.dumps(line) + "\n")
        self.out.flush()

    def pass_once(self, look_up=True):
        try:
            if look_up:
                self.look_up_total()
            self.write_line()
        except Exception:  # noqa: BLE001 - see the class comment
            pass

    def run(self):
        while True:
            self.pass_once()
            if self.stopping.wait(self.interval):
                break
        self.pass_once(look_up=False)  # The download is over; stop() does not wait for the Hub.

    def stop(self, wait=LAST_LINE_WAIT_SECONDS):
        self.stopping.set()
        self.join(wait)


def main(env=None, out=None, interval=INTERVAL_SECONDS):
    env = dict(os.environ if env is None else env)
    model = env["MODEL"]
    # The bars in a log are noise, and one without its line end would swallow the next progress line.
    child_env = dict(env, HF_HUB_DISABLE_PROGRESS_BARS="1")
    try:
        child = subprocess.Popen(["hf", "download", model], env=child_env)
    except OSError as error:
        print(f"download: cannot start hf: {error}", file=sys.stderr, flush=True)
        return EXIT_NO_HF
    # This is PID 1: without a handler SIGTERM does nothing, and the kubelet waits out the whole grace period.
    signal.signal(signal.SIGTERM, lambda *_: child.terminate())
    reporter = Reporter(model, env, out=out, interval=interval)
    reporter.start()
    code = child.wait()
    reporter.stop()
    return 128 - code if code < 0 else code


if __name__ == "__main__":
    # Not sys.exit: the Reporter may still be busy, and Python aborts with exit code 134 when a daemon thread holds the
    # stdout lock during shutdown. A finished download must not look failed. Reporter flushes what it writes.
    os._exit(main())
