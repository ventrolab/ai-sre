"""Messenger adapters. Each adapter implements base.Adapter."""

from __future__ import annotations

from .base import Adapter, Inbound


def load(names: list[str], inbound: Inbound) -> list[Adapter]:
    adapters: list[Adapter] = []
    for name in names:
        if name == "telegram":
            from .telegram import TelegramAdapter

            adapters.append(TelegramAdapter.from_env(inbound))
        elif name == "console":
            from .console import ConsoleAdapter

            adapters.append(ConsoleAdapter(inbound))
        else:
            raise ValueError(f"unknown chat adapter {name!r}")
    return adapters
