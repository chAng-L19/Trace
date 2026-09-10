from .bounded_output import BoundedOutput


# Compatibility name for the model streaming path. BoundedOutput is the only
# implementation so model, worker, and tool projections share one contract.
StreamTextAccumulator = BoundedOutput


__all__ = ["StreamTextAccumulator"]
