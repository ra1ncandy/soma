# OSWorld tasks, replicated on plain files

The `osworld` arena tier reuses tasks from [OSWorld](https://github.com/xlang-ai/OSWorld)
(Apache 2.0) without its desktop VM. A task keeps OSWorld's instruction, starting files and
checker function; the contestant edits the file in its workspace with code (Python libraries or
headless LibreOffice) instead of driving a GUI. Scores are not comparable with the OSWorld
leaderboard.

Only tasks whose checker reads files are used, and only those that `prepare.py` verifies: the
checker must reject the untouched starting files and accept OSWorld's gold file.

## Prepare (once)

```sh
ROOT=~/.local/share/soma-arena/osworld
git clone --depth 1 https://github.com/xlang-ai/OSWorld.git $ROOT/src
# Environment that runs OSWorld's checkers (their heavy GUI/OCR dependencies are not needed).
uv venv --python 3.12 $ROOT/venv
uv pip install --python $ROOT/venv/bin/python openpyxl pandas rapidfuzz lxml cssselect formulas \
  tldextract xmltodict python-docx odfpy Pillow numpy python-pptx scikit-image \
  opencv-python-headless pdfplumber pyyaml
# Libraries offered to the contestants, for the system Python.
uv pip install --target $ROOT/pylib --python /usr/bin/python3 openpyxl python-docx python-pptx
# Download files, validate every task, write $ROOT/tasks.json.
$ROOT/venv/bin/python internal/arena/osworld/prepare.py --root $ROOT
```

LibreOffice (`soffice`) must be installed on the host.

## Run

```sh
soma arena run --tier osworld --limit 20 ...        # or --tasks osw-calc-01b269ae,...
```

## Files

- `grade.py` grades one task: maps `/home/user/...` in the task to the workspace and calls
  OSWorld's metric function. Embedded into the soma binary.
- `prepare.py` builds the manifest of validated tasks.
