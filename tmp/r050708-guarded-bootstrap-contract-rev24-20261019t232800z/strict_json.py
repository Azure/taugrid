import json
from decimal import Decimal, InvalidOperation


class StrictJSONError(ValueError):
    pass


def _pairs(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise StrictJSONError(f"duplicate key: {key}")
        value[key] = item
    return value


def _constant(value):
    raise StrictJSONError(f"non-finite number: {value}")


def _decimal(value):
    try:
        parsed = Decimal(value)
    except InvalidOperation as error:
        raise StrictJSONError("invalid JSON number") from error
    if not parsed.is_finite():
        raise StrictJSONError("non-finite JSON number")
    return parsed


def loads(raw, label="JSON", maximum_bytes=None):
    if isinstance(raw, bytes):
        if maximum_bytes is not None and len(raw) > maximum_bytes:
            raise StrictJSONError(f"{label} exceeds byte ceiling")
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError as error:
            raise StrictJSONError(f"{label} is not UTF-8") from error
    elif isinstance(raw, str):
        text = raw
        if maximum_bytes is not None and len(text.encode("utf-8")) > maximum_bytes:
            raise StrictJSONError(f"{label} exceeds byte ceiling")
    else:
        raise StrictJSONError(f"{label} input type mismatch")
    try:
        return json.loads(
            text,
            object_pairs_hook=_pairs,
            parse_constant=_constant,
            parse_float=_decimal,
        )
    except StrictJSONError:
        raise
    except (json.JSONDecodeError, TypeError, ValueError) as error:
        raise StrictJSONError(f"{label} parse failed") from error
