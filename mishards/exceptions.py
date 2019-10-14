import mishards.exception_codes as codes


class BaseException(Exception):
    code = codes.INVALID_CODE
    message = 'BaseException'

    def __init__(self, message='', metadata=None):
        self.message = self.__class__.__name__ if not message else message
        self.metadata = metadata


class ConnectionConnectError(BaseException):
    code = codes.CONNECT_ERROR_CODE


class ConnectionNotFoundError(BaseException):
    code = codes.CONNECTTION_NOT_FOUND_CODE


class DBError(BaseException):
    code = codes.DB_ERROR_CODE


class TableNotFoundError(BaseException):
    code = codes.TABLE_NOT_FOUND_CODE


class InvalidArgumentError(BaseException):
    code = codes.INVALID_ARGUMENT_CODE


class InvalidRangeError(BaseException):
    code = codes.INVALID_DATE_RANGE_CODE
