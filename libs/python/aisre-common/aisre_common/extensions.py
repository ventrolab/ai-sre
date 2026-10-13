"""Extensions: packages that add features to a service without changing the core.

An extension package declares an entry point in one of the groups below; the service calls
it once at startup with its context. The core ships no extensions; editions and plugins
install theirs next to the service (e.g. `pip install` in a derived image).

    [project.entry-points."ventrolab.aisre.web"]
    audit-export = "my_package.web:register"      # register(app, cfg, db)

Groups:
    ventrolab.aisre.web         register(app: FastAPI, cfg, db: Callable[[], Store])
    ventrolab.aisre.controller  register(engine: Engine, cfg)
"""

from __future__ import annotations

import logging
from importlib.metadata import entry_points
from typing import Any

log = logging.getLogger("aisre.extensions")

WEB = "ventrolab.aisre.web"
CONTROLLER = "ventrolab.aisre.controller"


def load(group: str, *args: Any) -> list[str]:
    """Calls every extension of the group with args; returns their names.

    A failing extension stops the service: a half-loaded edition is worse than none."""
    names = []
    for ep in sorted(entry_points(group=group), key=lambda e: e.name):
        register = ep.load()
        register(*args)
        names.append(ep.name)
        log.info("extension loaded: %s (%s)", ep.name, ep.value)
    return names
