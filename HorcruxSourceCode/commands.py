# This function is used to model the commands
# Created by fuchen Ma in June, 2023

import json
from enum import Enum

class Relation(Enum):
    SELF = "self"
    FATHER = "father"
    SON = "son"
    NREL = "nrel"

class Ano(Enum):
    NORMAL = "normal"
    SUSPEND = "suspend"
    DELAY = "delay"
 

# <op, [Relation1, Relation2], Ano>
# <op, [Relation], Ano>
class Command:
    
    def __init__(self, opcode, relation, ano):
        self.opcode = opcode
        self.relation = relation
        self.ano = ano
    
    # use str(Command) for indexing
    def __str__(self):
        return f"{self.opcode}_{self.relation}_{self.ano}"


def read_json():
    json_file = open("./commands.json")
    commands = json.loads(json_file.read())
    return commands

def get_commands():
    commands = read_json()["commands"]
    return commands

def generate_root_command(opname,ano):
    # ano is the elements from the Enum Ano
    # Ano.NORMAL, Ano.SUSPEND, Ano.Delay
    commands = get_commands()
    param = []
    for command in commands:
        if command['name'] != opname:
            continue
        if command['argument_num'] == 1:
            param.append(Relation.SELF)
        else:
            param.append(Relation.SELF)
            param.append(Relation.SELF)
    return Command(opname, param, ano)
