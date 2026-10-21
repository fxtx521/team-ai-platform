"""错误应答工具：错误码只从契约包取，禁止在实现里硬编码数字。"""
from fastapi import HTTPException

from contracts.schema import ERROR_CODES


def fail(status_code: int, code_name: str, message: str) -> HTTPException:
    """统一抛错，body 里带契约错误码方便执行器分支处理。"""
    return HTTPException(
        status_code=status_code,
        detail={"code": ERROR_CODES[code_name], "code_name": code_name, "message": message},
    )
