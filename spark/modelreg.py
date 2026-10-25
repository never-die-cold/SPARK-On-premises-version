"""Model registry: loads and validates spark/models.yaml."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Optional

import yaml


@dataclass(frozen=True)
class ModelEntry:
    name: str
    url: str
    filename: str
    approx_gb: float
    ctx: int
    desc: str = ""

    @property
    def approx_bytes(self) -> int:
        return int(self.approx_gb * 1024**3)


class ModelRegistry:
    def __init__(self, models: dict[str, ModelEntry]):
        self._models = models

    @classmethod
    def load(cls, extra_path: Optional[str] = None) -> "ModelRegistry":
        here = Path(__file__).parent / "models.yaml"
        data = yaml.safe_load(here.read_text(encoding="utf-8")) or {}
        models: dict[str, ModelEntry] = {}
        for raw in data.get("models", []):
            entry = ModelEntry(
                name=raw["name"],
                url=raw["url"],
                filename=raw["filename"],
                approx_gb=float(raw["approx_gb"]),
                ctx=int(raw.get("ctx", 4096)),
                desc=raw.get("desc", ""),
            )
            if entry.name in models:
                raise ValueError(f"duplicate model name: {entry.name}")
            models[entry.name] = entry
        # optional user-provided additions (SPARK_MODELS_FILE)
        extra = extra_path or os.environ.get("SPARK_MODELS_FILE")
        if extra and Path(extra).exists():
            more = yaml.safe_load(Path(extra).read_text(encoding="utf-8")) or {}
            for raw in more.get("models", []):
                entry = ModelEntry(
                    name=raw["name"],
                    url=raw["url"],
                    filename=raw["filename"],
                    approx_gb=float(raw["approx_gb"]),
                    ctx=int(raw.get("ctx", 4096)),
                    desc=raw.get("desc", ""),
                )
                models[entry.name] = entry
        return cls(models)

    def get(self, name: str) -> Optional[ModelEntry]:
        return self._models.get(name)

    def names(self) -> list[str]:
        return sorted(self._models)

    def as_list(self) -> list[ModelEntry]:
        return [self._models[n] for n in self.names()]

    def __len__(self) -> int:
        return len(self._models)
