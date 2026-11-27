# OSWorld tasks, replicated on plain files

The `osworld` arena tier reuses tasks from [OSWorld](https://github.com/xlang-ai/OSWorld)
(Apache 2.0) without its desktop VM. A task keeps OSWorld's instruction, starting files and
checker function; the contestant produces the result file in its workspace with code and
command-line tools instead of driving a GUI. The workspace stands in for the task's home
directory (`/home/user`). Scores are not comparable with the OSWorld leaderboard.

What is included:

- Only tasks whose checker reads files, with no setup step that needs the VM.
- Only tasks `prepare.py` verifies: the checker must reject the untouched starting files and be
  passable. The pass side is shown with OSWorld's gold file when there is one, with the values a
  settings rule asks for, or with a held-out reference solution in `references.py`.
- No LibreOffice: its three task domains are left out, tasks elsewhere that involve it or
  produce office documents are skipped, and nothing here runs LibreOffice.
- No tasks that need the network (soma's sandbox is offline).
- No tasks whose checker expects exactly what the GUI would have written when the instruction
  does not say it (`NOT_FAITHFUL` in `prepare.py` lists them with the reason).

Today that is 15 tasks: 9 image edits (GIMP domain), 5 VS Code settings or text edits and 1
multi-app task (animated GIF from a video).

The workspace is mounted as the contestant's home directory. Hermes, which otherwise runs on the
host, is confined to a mount namespace for this (and every) tier: an earlier uncontained run
wrote task output, VS Code settings, pip packages and 1.3 GB of model downloads into the
operator's real home.

## Prepare (once)

```sh
ROOT=~/.local/share/soma-arena/osworld
git clone --depth 1 https://github.com/xlang-ai/OSWorld.git $ROOT/src
# Environment that runs OSWorld's checkers (their heavy GUI/OCR dependencies are not needed).
uv venv --python 3.12 $ROOT/venv
uv pip install --python $ROOT/venv/bin/python openpyxl pandas rapidfuzz lxml cssselect formulas \
  tldextract xmltodict python-docx odfpy Pillow numpy python-pptx scikit-image \
  opencv-python-headless pdfplumber pyyaml PyPDF2 mutagen
# Download files, validate every task, write $ROOT/tasks.json.
$ROOT/venv/bin/python internal/arena/osworld/prepare.py --root $ROOT
```

Contestants need Python 3 with Pillow, ImageMagick and ffmpeg on the host (under `/usr`, so
soma's sandbox sees them).

## Run

```sh
soma arena run --tier osworld ...        # or --tasks osw-image-72f83cdc,... / --limit N
```

## Files

- `grade.py` grades one task: maps `/home/user/...` in the task to the workspace and calls
  OSWorld's metric function. Embedded into the soma binary.
- `prepare.py` builds the manifest of validated tasks.
- `references.py` holds reference solutions used only by `prepare.py`.
